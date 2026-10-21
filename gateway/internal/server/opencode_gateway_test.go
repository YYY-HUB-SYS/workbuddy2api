package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/freegateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func openCodeTestHandler(t *testing.T, post http.HandlerFunc) *Handler {
	t.Helper()
	resetModelsCache()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"data":[{"id":"deepseek-v4.1-flash-free"}]}`)
			return
		}
		post(w, r)
	}))
	t.Cleanup(ts.Close)
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.Close)
	disabled := false
	manager := freegateway.New(freegateway.Config{Kilo: freegateway.ProviderConfig{Enabled: &disabled}}, freegateway.Options{OpenCodeBase: ts.URL})
	return NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "local-secret-key", FreeGateways: manager})
}

func TestOpenCodeGatewayRestoresCallerToolsAcrossJSONAndSSE(t *testing.T) {
	for _, sourceSSE := range []bool{false, true} {
		for _, clientStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("sourceSSE=%v/clientStream=%v", sourceSSE, clientStream), func(t *testing.T) {
				h := openCodeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer public" || !strings.HasPrefix(r.Header.Get("User-Agent"), "opencode/") {
						t.Errorf("missing OpenCode request identity")
					}
					for _, key := range []string{"Cookie", "X-Device-Token", "X-Forwarded-For"} {
						if r.Header.Get(key) != "" {
							t.Errorf("local header leaked: %s", key)
						}
					}
					data, _ := io.ReadAll(r.Body)
					var payload map[string]json.RawMessage
					if json.Unmarshal(data, &payload) != nil {
						t.Fatal("invalid outbound JSON")
					}
					if string(payload["metadata"]) != `{"large":9007199254740993}` {
						t.Error("lost exact request number")
					}
					if string(payload["stream"]) != "true" {
						t.Error("OpenCode upstream requires streaming even for a nonstream client")
					}
					var choice struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					}
					json.Unmarshal(payload["tool_choice"], &choice)
					if choice.Function.Name != "read_client" {
						t.Errorf("forced caller tool not renamed: %s", payload["tool_choice"])
					}
					var messages []struct {
						ToolCalls []struct {
							Function struct {
								Name string `json:"name"`
							} `json:"function"`
						} `json:"tool_calls"`
					}
					json.Unmarshal(payload["messages"], &messages)
					seenHistory := false
					for _, message := range messages {
						for _, call := range message.ToolCalls {
							if call.Function.Name == "read_client" {
								seenHistory = true
							}
						}
					}
					if !seenHistory {
						t.Error("prior caller tool history not renamed")
					}
					if sourceSSE {
						// Intentionally mislabeled and split the aliased name; one
						// tool's arguments arrive before its name is known.
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, "data: {\"id\":\"visible\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"hidden\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}},{\"index\":3,\"id\":\"actual\",\"type\":\"function\",\"function\":{\"arguments\":\"{\\\"value\\\":9007199254740993}\"}}]}}]}\n\n")
						io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":3,\"function\":{\"name\":\"read_\"}}]}}]}\n\n")
						io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":3,\"function\":{\"name\":\"client\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens\":7},\"provider_large\":9007199254740993}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"id":"visible","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"hidden","type":"function","function":{"name":"shell","arguments":"{}"}},{"id":"actual","type":"function","function":{"name":"read_client","arguments":"{\"value\":9007199254740993}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":7},"provider_large":9007199254740993}`)
					}
				})
				body := fmt.Sprintf(`{"model":"opencode:deepseek-v4.1-flash-free","stream":%v,"metadata":{"large":9007199254740993},"messages":[{"role":"user","content":"read seven"},{"role":"assistant","tool_calls":[{"id":"old","type":"function","function":{"name":"read","arguments":"{\"value\":7}"}}]},{"role":"tool","tool_call_id":"old","content":"7"}],"tools":[{"type":"function","function":{"name":"read","description":"caller tool","parameters":{"type":"object","properties":{"value":{"type":"integer"}}}}}],"tool_choice":{"type":"function","function":{"name":"read"}}}`, clientStream)
				w := freeRequest(h, "POST", "/v1/chat/completions", body)
				out := w.Body.String()
				if w.Code != 200 {
					t.Fatalf("HTTP %d: %s", w.Code, out)
				}
				for _, bad := range []string{"hidden", "read_client", `"name":"shell"`} {
					if strings.Contains(out, bad) {
						t.Errorf("internal compatibility data leaked (%s): %s", bad, out)
					}
				}
				for _, want := range []string{`"name":"read"`, `"id":"actual"`, "9007199254740993"} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %s: %s", want, out)
					}
				}
				if clientStream && (!strings.Contains(out, "data: [DONE]") || !strings.Contains(out, `"index":0`)) {
					t.Errorf("invalid restored stream: %s", out)
				}
				state := h.cfg.FreeGateways.Status()[0]
				if !state.InferenceVerified || !state.Available || h.live.snapshot().Completed != 7 {
					t.Fatalf("successful inference not accounted: %+v", state)
				}
			})
		}
	}
}

func TestOpenCodeGatewayPlaceholderOnlyNeverVerifiesInference(t *testing.T) {
	for _, sourceSSE := range []bool{false, true} {
		for _, clientStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("sourceSSE=%v/clientStream=%v", sourceSSE, clientStream), func(t *testing.T) {
				h := openCodeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					if sourceSSE {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"private_placeholder\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"private_placeholder","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
					}
				})
				w := freeRequest(h, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"opencode:deepseek-v4.1-flash-free","stream":%v,"messages":[{"role":"user","content":"Hello"}]}`, clientStream))
				if w.Code == 200 && !strings.Contains(w.Body.String(), `"error"`) {
					t.Fatalf("placeholder-only completion claimed success: %s", w.Body)
				}
				if strings.Contains(w.Body.String(), "private_placeholder") {
					t.Error("hidden placeholder call leaked")
				}
				state := h.cfg.FreeGateways.Status()[0]
				if state.InferenceVerified || state.Available || state.LastError == "" {
					t.Fatalf("failed output marked available: %+v", state)
				}
			})
		}
	}
}
