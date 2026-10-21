package freegateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// These fixtures were calculated independently from the pinned reference's
// FINGERPRINT_TOOL_DEFINITIONS and canonicalizeSession, not from this adapter.
func TestOpenCodeCanonicalWireFixtures(t *testing.T) {
	for _, tc := range []struct{ name, body, hash string }{
		{"read", openCodeReadTool, "523c98f17669a5f9838866ac0739c48406c9501d875421dbb8cc4e5b129aeb55"},
		{"shell", openCodeShellTool, "aa8c3960d557a8157b5987440251bea10c7e9e540747499455666b9bda2abc84"},
	} {
		sum := sha256.Sum256([]byte(tc.body))
		if hex.EncodeToString(sum[:]) != tc.hash {
			t.Errorf("%s definition no longer matches the reference", tc.name)
		}
	}
	if got := canonicalOpenCodeSession("conversation\x00stable-001"); got != "ses_80f94dd60ffe5pgOc4myHQV50o" {
		t.Fatalf("canonical session differs from reference: %s", got)
	}
}

func decodeOpenCodeTestPayload(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestPrepareOpenCodePreservesCallerToolsAndHistory(t *testing.T) {
	payload := decodeOpenCodeTestPayload(t, `{
		"model":"deepseek-v4.1-flash-free","large":9007199254740993,"stream":false,
		"stream_options":{"include_usage":true,"custom":9007199254740993},
		"unknown":{"integer":18446744073709551615},
		"metadata":{"conversation_id":"stable-001","custom":9007199254740993},
		"tools":[
			{"type":"function","function":{"name":"read_client","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"read_client2","parameters":{"type":"object"}}},
			{"type":"function","strict":true,"custom":9007199254740993,"function":{"name":"read","description":"Caller read","parameters":{"type":"object","properties":{"id":{"maximum":9007199254740993}}}}},
			{"type":"function","function":{"name":"read","description":"Duplicate must go"}},
			`+openCodeShellTool+`,
			{"type":"function","function":{"name":"Read","description":"Capital preserved"}},
			{"type":"function","function":{"name":"Read","description":"Duplicate unrelated preserved"}}
		],
		"tool_choice":{"type":"function","function":{"name":"read"},"unknown":9007199254740993},
		"messages":[
			{"role":"system","content":"Original policy","cache_control":{"integer":9007199254740993}},
			{"role":"user","content":[{"type":"text","text":"Read my file"}]},
			{"role":"assistant","content":[{"type":"text","text":"Working"}],"tool_calls":[{"id":"call-01","type":"function","function":{"name":"read","arguments":"{\"id\":9007199254740993}"},"unknown":9007199254740993}]},
			{"role":"tool","tool_call_id":"call-01","content":"Caller result","unknown":9007199254740993}
		]
	}`)
	originalUnknown := string(payload["unknown"])
	plan, session, err := prepareOpenCode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if session != "ses_80f94dd60ffe5pgOc4myHQV50o" || plan.Aliases["read_client3"] != "read" || !plan.Injected["read"] || plan.Injected["shell"] || !plan.Declared["read"] || !plan.Declared["shell"] {
		t.Fatalf("incorrect identity or alias plan: %s %+v", session, plan)
	}
	if string(payload["large"]) != "9007199254740993" || string(payload["unknown"]) != originalUnknown || !strings.Contains(string(payload["metadata"]), "9007199254740993") {
		t.Fatal("unknown fields or integer precision changed")
	}
	if string(payload["stream"]) != "true" || !strings.Contains(string(payload["stream_options"]), `"custom":9007199254740993`) {
		t.Fatal("OpenCode must use upstream SSE without losing caller stream options")
	}
	var tools []json.RawMessage
	json.Unmarshal(payload["tools"], &tools)
	if len(tools) != 7 {
		t.Fatalf("only reserved duplicate should be removed and one read injected: %s", payload["tools"])
	}
	want := []string{"read_client", "read_client2", "read_client3", "shell", "Read", "Read", "read"}
	for i, tool := range tools {
		if got := toolName(tool); got != want[i] {
			t.Errorf("tool %d got %q want %q", i, got, want[i])
		}
	}
	if !strings.Contains(string(tools[2]), `"custom":9007199254740993`) || !strings.Contains(string(tools[2]), `"strict":true`) || !strings.Contains(string(tools[2]), `"maximum":9007199254740993`) {
		t.Fatalf("renamed tool lost fields: %s", tools[2])
	}
	if !canonicalDefinition(tools[3], json.RawMessage(openCodeShellTool)) || !canonicalDefinition(tools[6], json.RawMessage(openCodeReadTool)) {
		t.Fatal("canonical read/shell definitions missing")
	}
	choice := rawObject(payload["tool_choice"])
	if rawString(rawObject(choice["function"])["name"]) != "read_client3" || string(choice["unknown"]) != "9007199254740993" {
		t.Fatalf("tool_choice alias or precision broken: %s", payload["tool_choice"])
	}
	var messages []json.RawMessage
	json.Unmarshal(payload["messages"], &messages)
	if len(messages) != 4 || !strings.Contains(string(messages[0]), "Do not call the `read` tool") || strings.Contains(string(messages[0]), "`shell` tools") {
		t.Fatalf("placeholder note is incorrect: %s", payload["messages"])
	}
	var calls []json.RawMessage
	json.Unmarshal(rawObject(messages[2])["tool_calls"], &calls)
	if toolName(calls[0]) != "read_client3" || !strings.Contains(string(calls[0]), "9007199254740993") || !strings.Contains(string(messages[2]), `"content":[{"type":"text","text":"Working"}]`) || !strings.Contains(string(messages[3]), `"tool_call_id":"call-01"`) {
		t.Fatalf("history alias lost caller data: %s", payload["messages"])
	}
}

func TestPrepareOpenCodeNotesOnlyInjectedToolsAndRemovesNone(t *testing.T) {
	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		payload := decodeOpenCodeTestPayload(t, `{"tool_choice":`+choice+`,"tools":[`+openCodeReadTool+`,`+openCodeShellTool+`],"messages":[{"role":"system","content":"Keep exactly"},{"role":"user","content":"Hi"}]}`)
		before := string(payload["messages"])
		plan, _, err := prepareOpenCode(payload)
		if err != nil || len(plan.Aliases) != 0 || len(plan.Injected) != 0 {
			t.Fatalf("existing canonical declarations changed: %+v %v", plan, err)
		}
		if _, exists := payload["tool_choice"]; exists || string(payload["messages"]) != before {
			t.Fatal("none was forwarded or unchanged messages were rewritten")
		}
	}
	payload := decodeOpenCodeTestPayload(t, `{"tool_choice":{"type":"function","function":{"name":"ping"}},"messages":[{"role":"system","content":[{"type":"text","text":"Keep structured system"}]},{"role":"user","content":"Hi"}]}`)
	before := string(payload["tool_choice"])
	plan, _, err := prepareOpenCode(payload)
	if err != nil || !plan.Injected["read"] || !plan.Injected["shell"] || string(payload["tool_choice"]) != before {
		t.Fatalf("tool choice changed without an alias: %+v %v", plan, err)
	}
	var messages []json.RawMessage
	json.Unmarshal(payload["messages"], &messages)
	if len(messages) != 3 || !strings.Contains(string(messages[1]), "Keep structured system") || !strings.Contains(rawString(rawObject(messages[0])["content"]), "`read` or `shell`") {
		t.Fatalf("structured system prompt overwritten: %s", payload["messages"])
	}
}

func TestOpenCodeSessionStaysWithConversation(t *testing.T) {
	prepare := func(body string) string {
		t.Helper()
		_, session, err := prepareOpenCode(decodeOpenCodeTestPayload(t, body))
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	first := prepare(`{"messages":[{"role":"system","content":"Policy"},{"role":"user","content":"Sensitive initial prompt"}]}`)
	next := prepare(`{"model":"a-different-model","messages":[{"role":"system","content":"Policy"},{"role":"user","content":"Sensitive initial prompt"},{"role":"assistant","content":"Response"},{"role":"user","content":"Next turn"}]}`)
	if first != next || !regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`).MatchString(first) || strings.Contains(first, "Sensitive") {
		t.Fatalf("unstable or noncanonical private session: %s %s", first, next)
	}
	if first == prepare(`{"messages":[{"role":"system","content":"Policy"},{"role":"user","content":"Different conversation"}]}`) || first == prepare(`{"messages":[{"role":"system","content":"Other policy"},{"role":"user","content":"Sensitive initial prompt"}]}`) {
		t.Fatal("different conversation seeds collided")
	}
	one := prepare(`{"session_id":"session with\nprivate data","messages":[{"role":"user","content":"One"}]}`)
	two := prepare(`{"metadata":{"session_id":"session with\nprivate data"},"messages":[{"role":"user","content":"Two"}]}`)
	if one != two || one == first {
		t.Fatalf("explicit session not preferred: %s %s", one, two)
	}
}

func TestOpenCodeHeadersAndProviderIsolation(t *testing.T) {
	var openSession string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isOpen := strings.HasPrefix(r.URL.Path, "/opencode/")
		for _, forbidden := range []string{"Cookie", "X-Api-Key", "X-Device-Token", "X-Opencode-Request"} {
			if r.Header.Get(forbidden) != "" {
				t.Errorf("unexpected upstream header %s", forbidden)
			}
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("User-Agent") != "workbuddy2api-freegateway" || r.Header.Get("X-Opencode-Session") != "" {
				t.Error("chat fingerprint leaked into catalog discovery")
			}
			if isOpen {
				io.WriteString(w, `{"data":[{"id":"deepseek-v4.1-flash-free"}]}`)
			} else {
				io.WriteString(w, `{"data":[{"id":"kilo-auto/free","isFree":true}]}`)
			}
			return
		}
		data, _ := io.ReadAll(r.Body)
		payload := decodeOpenCodeTestPayload(t, string(data))
		if isOpen {
			if r.Header.Get("Authorization") != "Bearer public" || r.Header.Get("User-Agent") != openCodeClientUA || r.Header.Get("X-Opencode-Client") != "cli" || r.Header.Get("X-Opencode-Project") != "global" || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("missing v2 headers: %v", r.Header)
			}
			session := r.Header.Get("X-Opencode-Session")
			if openSession != "" && session != openSession {
				t.Error("same conversation minted a different session")
			}
			openSession = session
			if session == "" || strings.Contains(session, "private") {
				t.Error("missing or unencoded session")
			}
			for _, header := range []string{"X-Opencode-Session-Id", "X-Session-Id", "X-Session-Affinity"} {
				if r.Header.Get(header) != session {
					t.Errorf("session header %s mismatch", header)
				}
			}
			if rawString(payload["model"]) != "deepseek-v4.1-flash-free" || string(payload["large"]) != "9007199254740993" || len(payload["tool_choice"]) != 0 || string(payload["stream"]) != "true" {
				t.Errorf("request adaptation invalid: %s", data)
			}
			if string(payload["stream"]) == "true" {
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Error("stream accept mismatch")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				return
			}
		} else {
			if r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") != "workbuddy2api-freegateway" || r.Header.Get("X-Session-Id") != "" || r.Header.Get("X-Opencode-Client") != "" {
				t.Errorf("OpenCode headers leaked to Kilo: %v", r.Header)
			}
			if string(payload["tools"]) != "[]" || string(payload["tool_choice"]) != `"none"` || string(payload["messages"]) != `[{"role":"user","content":"Kilo unchanged"}]` {
				t.Errorf("OpenCode payload adaptation leaked to Kilo: %s", data)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()
	m := New(Config{}, Options{OpenCodeBase: ts.URL + "/opencode", KiloBase: ts.URL + "/kilo"})
	for _, stream := range []bool{false, true} {
		body := `{"model":"ignored","session_id":"private session","large":9007199254740993,"messages":[{"role":"user","content":"Hi"}],"tool_choice":"none","stream":` + string(rawJSON(stream)) + `}`
		resp, err := m.Chat(context.Background(), "opencode:deepseek-v4.1-flash-free", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	resp, err := m.Chat(context.Background(), "kilo:kilo-auto/free", []byte(`{"tools":[],"tool_choice":"none","messages":[{"role":"user","content":"Kilo unchanged"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestPrepareOpenCodeRejectsMalformedCollections(t *testing.T) {
	for _, body := range []string{`{"messages":{}}`, `{"tools":"invalid"}`} {
		if _, _, err := prepareOpenCode(decodeOpenCodeTestPayload(t, body)); err == nil {
			t.Errorf("malformed collection silently lost: %s", body)
		}
	}
}
