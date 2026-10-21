package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/freegateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func freeTestHandler(t *testing.T, fake http.HandlerFunc) (*Handler, *httptest.Server) {
	t.Helper()
	resetModelsCache()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(p.Close)
	disabled := false
	m := freegateway.New(freegateway.Config{OpenCode: freegateway.ProviderConfig{Enabled: &disabled}}, freegateway.Options{KiloBase: ts.URL})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "local-secret-key", FreeGateways: m})
	return h, ts
}

func freeRequest(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer local-secret-key")
	r.Header.Set("Cookie", "workbuddy-token=private")
	r.Header.Set("X-Device-Token", "device-secret")
	r.Header.Set("X-Forwarded-For", "192.168.1.9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestFreeGatewayAccountlessModelsHealthStatusAndSyncUsage(t *testing.T) {
	var posts atomic.Int32
	h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"cohere/north-mini-code:free","isFree":true},{"id":"paid","isFree":false}]}`)
			return
		}
		posts.Add(1)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("bad chat path %s", r.URL.Path)
		}
		for _, header := range []string{"Authorization", "Cookie", "X-Device-Token", "X-Forwarded-For"} {
			if r.Header.Get(header) != "" {
				t.Errorf("credential/header leak: %s", header)
			}
		}
		data, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		json.Unmarshal(data, &payload)
		if string(payload["model"]) != `"cohere/north-mini-code:free"` || string(payload["metadata"]) != `{"large":9007199254740993}` || string(payload["tools"]) != `[{"type":"function","function":{"name":"my_tool","parameters":{}}}]` || string(payload["reasoning"]) != `{"enabled":false}` {
			t.Errorf("altered request: %s", data)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "private=1")
		w.Header().Set("X-Upstream-Secret", "private")
		io.WriteString(w, `{"id":"real-id","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"OK","reasoning_details":[{"text":"thought"}],"tool_calls":[]},"finish_reason":"stop"}],"usage":{"completion_tokens":7},"provider_extra":true}`)
	})
	if w := freeRequest(h, "GET", "/healthz", ""); w.Code != 200 {
		t.Fatalf("accountless health: %d %s", w.Code, w.Body)
	}
	w := freeRequest(h, "GET", "/v1/models", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "kilo:cohere/north-mini-code:free") || strings.Contains(w.Body.String(), `"id":"kilo:paid"`) {
		t.Fatalf("accountless models: %d %s", w.Code, w.Body)
	}
	w = freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:cohere/north-mini-code:free","stream":false,"messages":[{"role":"user","content":"OK"}],"metadata":{"large":9007199254740993},"reasoning":{"enabled":false},"tools":[{"type":"function","function":{"name":"my_tool","parameters":{}}}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"provider_extra":true`) || !strings.Contains(w.Body.String(), `"reasoning_details"`) {
		t.Fatalf("sync response: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Upstream-Secret") != "" {
		t.Fatal("upstream headers leaked")
	}
	usage := h.live.snapshot()
	if usage.Active != 0 || usage.Completed != 7 {
		t.Fatalf("free sync telemetry missing: %+v", usage)
	}
	w = freeRequest(h, "GET", "/status", "")
	if !strings.Contains(w.Body.String(), `"free_gateways"`) || !strings.Contains(w.Body.String(), `"model_count":1`) {
		t.Fatalf("missing status: %s", w.Body)
	}
	w = freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:paid","messages":[]}`)
	if w.Code != 404 || posts.Load() != 1 {
		t.Fatalf("paid forwarded: %d posts=%d", w.Code, posts.Load())
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"kilo:cohere/north-mini-code:free"}`)))
	if unauth.Code != 401 || posts.Load() != 1 {
		t.Fatal("local auth bypass")
	}
}

func TestFreeGatewaySSEPreservesBytesAndLiveEstimates(t *testing.T) {
	first := ": KILO PROCESSING\n\ndata:{\"choices\":[{\"delta\":{\"content\":\"hello\",\"reasoning\":\"thinking\",\"reasoning_details\":[{\"text\":\"original\"}],\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"custom\",\"arguments\":\"{}\"}}]}}]}\n\n"
	last := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"
	release := make(chan struct{})
	h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, last)
	})
	local := httptest.NewServer(h)
	defer local.Close()
	req, _ := http.NewRequest("POST", local.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kilo:north:free","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer local-secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	prefix := make([]byte, len(first))
	if _, err := io.ReadFull(reader, prefix); err != nil {
		close(release)
		t.Fatal(err)
	}
	if string(prefix) != first {
		t.Fatalf("SSE altered: %q", prefix)
	}
	usage := h.live.snapshot()
	if usage.Active != 1 || usage.Estimated < 1 || usage.Observed <= 0 {
		close(release)
		t.Fatalf("no active free SSE usage: %+v", usage)
	}
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil || string(tail) != last {
		t.Fatalf("tail altered: %q, %v", tail, err)
	}
	usage = h.live.snapshot()
	if usage.Active != 0 || usage.Completed != 4 {
		t.Fatalf("SSE completion accounting: %+v", usage)
	}
}

func TestFreeGatewayNonstreamSSEFallbackAndMissingUsage(t *testing.T) {
	h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"real\",\"model\":\"north:free\",\"choices\":[{\"delta\":{\"content\":\"hello\",\"reasoning_content\":\"thought\"}}]}\n\ndata: [DONE]\n\n")
	})
	w := freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:north:free","stream":false,"messages":[]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"hello"`) || !strings.Contains(w.Body.String(), `"reasoning_content":"thought"`) {
		t.Fatalf("SSE sync aggregate: %d %s", w.Code, w.Body)
	}
	if usage := h.live.snapshot(); usage.Completed != 0 || usage.Active != 0 {
		t.Fatalf("missing usage was invented: %+v", usage)
	}
}

func TestFreeGatewayInterruptAndInvalidResponse(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "application/json"} {
		t.Run(contentType, func(t *testing.T) {
			h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
					return
				}
				w.Header().Set("Content-Type", contentType)
			})
			w := freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:north:free","stream":true,"messages":[]}`)
			if w.Code != 502 || !strings.Contains(w.Body.String(), "free_gateway_invalid_response") {
				t.Fatalf("empty/invalid response accepted: %d %s", w.Code, w.Body)
			}
			if h.cfg.FreeGateways.Status()[1].LastError == "" {
				t.Fatal("error missing from provider status")
			}
		})
	}
}

func TestFreeGatewayLoggingUsesExactAndMissingTokenUsage(t *testing.T) {
	st := newChatStat(time.Now().Add(-time.Millisecond), []byte(`{"model":"kilo:north:free"}`), true)
	observer := &freeStreamObserver{reader: strings.NewReader("data:{\"choices\":[]}\n\ndata:{\"usage\":{\"completion_tokens\":0}}\n\n"), st: st}
	io.Copy(io.Discard, observer)
	if st.ttfb <= 0 || st.toks != 0 {
		t.Fatalf("exact zero usage missing: %+v", st)
	}
	st = newChatStat(time.Now(), []byte(`{"model":"kilo:north:free"}`), true)
	observer = &freeStreamObserver{reader: strings.NewReader("data:{\"choices\":[]}\n\n"), st: st}
	io.Copy(io.Discard, observer)
	if st.toks != -1 {
		t.Fatal("missing usage became zero")
	}
}

func TestFreeGatewayTruncatedStreamsAreNotSuccessful(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, payload := range []string{
			": KILO PROCESSING\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
			"data: {\"error\":{\"code\":\"provider_failed\"}}\n\n",
		} {
			h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, payload)
			})
			requestBody := `{"model":"kilo:north:free","stream":false}`
			if stream {
				requestBody = `{"model":"kilo:north:free","stream":true}`
			}
			w := freeRequest(h, "POST", "/v1/chat/completions", requestBody)
			if stream {
				if !strings.Contains(w.Body.String(), `"error"`) {
					t.Fatalf("truncated stream claimed success: %s", w.Body)
				}
			} else if w.Code != 502 {
				t.Fatalf("failed SSE aggregate became success: %d %s", w.Code, w.Body)
			}
			status := h.cfg.FreeGateways.Status()[1]
			if status.LastError == "" || status.Available || status.InferenceVerified {
				t.Fatalf("failed inference claimed available: %+v", status)
			}
		}
	}
}

func TestFreeGatewayAggregatePreservesKiloReasoningAndTools(t *testing.T) {
	h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		// Actual stream shape wins over a proxy's incorrect Content-Type.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "data:{\"id\":\"original\",\"model\":\"north:free\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"think\",\"reasoning_details\":[{\"text\":\"detail\"}],\"tool_calls\":[{\"index\":0,\"id\":\"call_original\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n")
		io.WriteString(w, "data:{\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\" more\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Paris\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens\":9,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\ndata: [DONE]\n\n")
	})
	w := freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:north:free","stream":false}`)
	if w.Code != 200 {
		t.Fatalf("aggregate failed: %d %s", w.Code, w.Body)
	}
	for _, value := range []string{`"reasoning":"think more"`, `"reasoning_details":[{"text":"detail"}]`, `"name":"get_weather"`, `"arguments":"{\"city\":\"Paris\"}"`, `"id":"call_original"`, `"completion_tokens_details":{"reasoning_tokens":3}`} {
		if !strings.Contains(w.Body.String(), value) {
			t.Errorf("missing preserved aggregate %s: %s", value, w.Body)
		}
	}
	if h.live.snapshot().Completed != 9 || !h.cfg.FreeGateways.Status()[1].InferenceVerified {
		t.Fatal("successful completed inference not observed")
	}
}

func TestFreeGatewayRateLimitRetryAfter(t *testing.T) {
	h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"secret internal error"}}`)
	})
	w := freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:north:free"}`)
	if w.Code != 429 || w.Header().Get("Retry-After") != "42" || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("rate limit response unsafe: %d %+v %s", w.Code, w.Header(), w.Body)
	}
}

func TestFreeGatewayJSONStreamFallbackAddsToolIndices(t *testing.T) {
	h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":3}}`)
	})
	w := freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:north:free","stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"index":1`) || strings.Contains(w.Body.String(), `"message":`) || !strings.Contains(w.Body.String(), `"delta":`) || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("invalid JSON stream conversion: %d %s", w.Code, w.Body)
	}
}

func TestFreeGatewayInvalidJSONChoicesNeverVerifyInference(t *testing.T) {
	for _, choices := range []string{`null`, `"invalid"`, `[]`} {
		h, _ := freeTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":`+choices+`}`)
		})
		w := freeRequest(h, "POST", "/v1/chat/completions", `{"model":"kilo:north:free","stream":false}`)
		if w.Code != 502 || h.cfg.FreeGateways.Status()[1].InferenceVerified {
			t.Fatalf("malformed choices verified: %d %s", w.Code, w.Body)
		}
	}
}
