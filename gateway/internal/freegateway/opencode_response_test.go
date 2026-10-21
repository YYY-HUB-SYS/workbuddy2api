package freegateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func responsePlan() *openCodePlan {
	return &openCodePlan{
		Aliases:  map[string]string{"read_client": "read"},
		Declared: map[string]bool{"read": true, "weather": true},
		Injected: map[string]bool{"read": true, "shell": true},
	}
}

func responseFor(body string, contentType string) *http.Response {
	return &http.Response{Header: http.Header{"Content-Type": {contentType}, "Content-Length": {"123"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func readAdapted(t *testing.T, body string, plan *openCodePlan, contentType string) string {
	t.Helper()
	resp := responseFor(body, contentType)
	if err := adaptOpenCodeResponse(resp, plan); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 {
		t.Fatal("stale content length retained")
	}
	return string(data)
}

func TestOpenCodeJSONRestoresAliasesAndFiltersOnlyInjectedTools(t *testing.T) {
	body := `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_details":[{"text":"thought"}],"tool_calls":[{"id":"private","type":"function","function":{"name":"read","arguments":"{\"secret\":1}"}},{"id":"visible","type":"function","function":{"name":"read_client","arguments":"{\"path\":\"a\"}"},"extra":9007199254740993}]} ,"finish_reason":"tool_calls"}],"usage":{"completion_tokens":9007199254740993},"provider_extra":9007199254740995}`
	got := readAdapted(t, body, responsePlan(), "text/event-stream")
	if strings.Contains(got, "private") || strings.Contains(got, "secret") || strings.Contains(got, "read_client") {
		t.Fatalf("injected call or wire alias leaked: %s", got)
	}
	for _, want := range []string{`"name":"read"`, `"id":"visible"`, `"reasoning_details":[{"text":"thought"}]`, `"completion_tokens":9007199254740993`, `"provider_extra":9007199254740995`, `"extra":9007199254740993`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	canonical := &openCodePlan{Declared: map[string]bool{"read": true}}
	got = readAdapted(t, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, canonical, "application/json")
	if !strings.Contains(got, `"name":"read"`) {
		t.Fatal("caller-declared canonical read was removed")
	}
}

func TestOpenCodeJSONHiddenOnlyAndTextFinish(t *testing.T) {
	for _, text := range []string{"null", `""`, `"  "`, `[]`, `[{"type":"text","text":""}]`, `[{"type":"text","text":"  "}]`} {
		resp := responseFor(`{"choices":[{"message":{"content":`+text+`,"reasoning_content":"thought","tool_calls":[{"function":{"name":"shell","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, "application/json")
		err := adaptOpenCodeResponse(resp, responsePlan())
		var gateway *Error
		if !errors.As(err, &gateway) || gateway.Status != 502 || gateway.Code != "opencode_hidden_tool_response" {
			t.Fatalf("expected structured hidden-only error, got %v", err)
		}
	}
	got := readAdapted(t, `{"choices":[{"message":{"content":"hello","tool_calls":[{"function":{"name":"shell","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":7}}`, responsePlan(), "application/json")
	if !strings.Contains(got, `"finish_reason":"stop"`) || !strings.Contains(got, `"tool_calls":[]`) {
		t.Fatalf("bad finish after hidden tool: %s", got)
	}
}

func sseEvent(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func sseTools(choice, index int, function map[string]any, extra map[string]any) string {
	call := map[string]any{"index": index, "function": function}
	for key, value := range extra {
		call[key] = value
	}
	return sseEvent(map[string]any{"choices": []any{map[string]any{"index": choice, "delta": map[string]any{"tool_calls": []any{call}}}}})
}

func parseSSE(t *testing.T, body string) []map[string]json.RawMessage {
	t.Helper()
	var frames []map[string]json.RawMessage
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") || strings.TrimSpace(strings.TrimPrefix(line, "data:")) == "[DONE]" {
			continue
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

type collectedTool struct{ name, id, arguments string }

func collectSSETools(t *testing.T, frames []map[string]json.RawMessage) map[int]map[int]*collectedTool {
	t.Helper()
	out := make(map[int]map[int]*collectedTool)
	for _, frame := range frames {
		var choices []struct {
			Index int `json:"index"`
			Delta struct {
				Tools []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(frame["choices"], &choices); err != nil && frame["choices"] != nil {
			t.Fatal(err)
		}
		for _, choice := range choices {
			if out[choice.Index] == nil {
				out[choice.Index] = make(map[int]*collectedTool)
			}
			for _, tool := range choice.Delta.Tools {
				current := out[choice.Index][tool.Index]
				if current == nil {
					current = &collectedTool{}
					out[choice.Index][tool.Index] = current
				}
				current.name += tool.Function.Name
				current.id += tool.ID
				current.arguments += tool.Function.Arguments
			}
		}
	}
	return out
}

func TestOpenCodeSSEBuffersNamesArgsAndRenumbersPerChoice(t *testing.T) {
	body := ": heartbeat\n\n" +
		sseTools(0, 2, map[string]any{"arguments": "{\"path\":"}, map[string]any{"id": "call_"}) +
		sseTools(0, 2, map[string]any{"name": "rea"}, map[string]any{"id": "a"}) +
		sseTools(0, 5, map[string]any{"name": "shell", "arguments": "{\"private\":1}"}, nil) +
		sseTools(0, 9, map[string]any{"name": "weather", "arguments": "{\"city\":"}, map[string]any{"id": "call_b"}) +
		sseTools(1, 12, map[string]any{"name": "weather", "arguments": "{}"}, map[string]any{"id": "call_c"}) +
		sseTools(0, 2, map[string]any{"name": "d_cli", "arguments": "\"a\""}, nil) +
		sseTools(0, 9, map[string]any{"arguments": "\"Paris\"}"}, nil) +
		sseTools(0, 2, map[string]any{"name": "ent", "arguments": "}"}, nil) +
		sseEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "thought"}, "finish_reason": "tool_calls"}, map[string]any{"index": 1, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"completion_tokens": json.Number("9007199254740993")}, "extra": json.Number("9007199254740995")}) +
		"data: [DONE]\n\n"
	got := readAdapted(t, body, responsePlan(), "application/json")
	if !strings.Contains(got, ": heartbeat\n") || strings.Contains(got, "private") || strings.Contains(got, "read_client") || strings.Contains(got, `"name":"rea"`) || strings.Contains(got, "shell") {
		t.Fatalf("stream leaked hidden/partial tools or lost heartbeat: %s", got)
	}
	for _, want := range []string{`"reasoning_content":"thought"`, `"completion_tokens":9007199254740993`, `"extra":9007199254740995`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	tools := collectSSETools(t, parseSSE(t, got))
	if len(tools[0]) != 2 || len(tools[1]) != 1 || tools[0][0].name != "weather" || tools[0][1].name != "read" || tools[1][0].name != "weather" {
		t.Fatalf("bad dense indices or names: %#v", tools)
	}
	if *tools[0][1] != (collectedTool{name: "read", id: "call_a", arguments: `{"path":"a"}`}) || tools[0][0].arguments != `{"city":"Paris"}` {
		t.Fatalf("lost buffered arguments/id: %#v %#v", tools[0][1], tools[0][0])
	}
}

func TestOpenCodeSSEHiddenOnlyErrorAndTextStop(t *testing.T) {
	for _, withFinish := range []bool{true, false} {
		body := sseTools(0, 3, map[string]any{"name": "shell", "arguments": "{}"}, nil)
		if withFinish {
			body += `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":4}}` + "\n\n"
		}
		body += "data: [DONE]\n\n"
		got := readAdapted(t, body, responsePlan(), "text/plain")
		if !strings.Contains(got, `"code":"opencode_hidden_tool_response"`) || strings.Contains(got, `"finish_reason":"tool_calls"`) || strings.Contains(got, `"name":"shell"`) {
			t.Fatalf("hidden-only call appeared successful: %s", got)
		}
	}
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" + sseTools(0, 3, map[string]any{"name": "shell", "arguments": "{}"}, nil) + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	got := readAdapted(t, body, responsePlan(), "text/event-stream")
	if !strings.Contains(got, `"finish_reason":"stop"`) || strings.Contains(got, `"error"`) {
		t.Fatalf("text plus hidden tool has wrong finish: %s", got)
	}
}

func TestOpenCodeSSEEmptyContentPartsAreNotAnswers(t *testing.T) {
	body := sseTools(0, 3, map[string]any{"name": "shell", "arguments": "{}"}, nil) +
		`data: {"choices":[{"index":0,"delta":{"content":[{"type":"text","text":" "}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	got := readAdapted(t, body, responsePlan(), "text/event-stream")
	if !strings.Contains(got, `"code":"opencode_hidden_tool_response"`) || strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("empty content parts made hidden tool appear successful: %s", got)
	}
}

func TestOpenCodeSSEInterruptedAndBounded(t *testing.T) {
	for _, body := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"data: {\"choices\":[",
		sseTools(0, 2, map[string]any{"arguments": "{}"}, nil),
	} {
		resp := responseFor(body, "text/event-stream")
		if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
			t.Fatal(err)
		}
		_, err := io.ReadAll(resp.Body)
		if err == nil {
			t.Fatalf("interrupted response accepted: %s", body)
		}
	}
	resp := responseFor("data: "+strings.Repeat("x", openCodeEventLimit+1), "text/event-stream")
	if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("oversized SSE frame accepted")
	}
}

func TestOpenCodeSSEIncompleteAliasNeverLeaks(t *testing.T) {
	for _, terminal := range []string{
		"data: [DONE]\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
	} {
		resp := responseFor(sseTools(0, 3, map[string]any{"name": "read_cli", "arguments": "{}"}, nil)+terminal, "text/event-stream")
		if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		if err == nil && !strings.Contains(string(data), `"error"`) {
			t.Fatal("incomplete alias appeared successful")
		}
		if strings.Contains(string(data), "read_cli") || strings.Contains(string(data), `"tool_calls":[`) {
			t.Fatalf("incomplete wire alias leaked: %s", data)
		}
	}
}

func TestOpenCodeSSEAllObservedChoicesMustFinish(t *testing.T) {
	body := sseTools(1, 3, map[string]any{"name": "read_cli", "arguments": "{}"}, nil) +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n"
	resp := responseFor(body, "text/event-stream")
	if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(string(got), "read_cli") {
		t.Fatalf("unfinished alternate choice accepted: %s, %v", got, err)
	}
}

func TestOpenCodeSSERejectsMalformedIndices(t *testing.T) {
	for _, index := range []string{`"large-string-index"`, `null`, `-1`, `2147483648`, `9007199254740993`} {
		for _, body := range []string{
			`data: {"choices":[{"index":` + index + `,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":` + index + `,"function":{"name":"read_client","arguments":"{}"}}]}}]}` + "\n\n",
		} {
			resp := responseFor(body, "text/event-stream")
			if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Fatalf("malformed index accepted: %s", index)
			}
		}
	}
}

func TestOpenCodeSSENullEntriesReturnErrors(t *testing.T) {
	for _, body := range []string{
		"data: {\"choices\":[null]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[null]}}]}\n\n",
		sseTools(0, 0, map[string]any{"name": "weather"}, nil) + "data: {\"choices\":[{\"delta\":{\"tool_calls\":[null]}}]}\n\n",
	} {
		resp := responseFor(body, "text/event-stream")
		if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Fatalf("null entry accepted: %s", body)
		}
	}
}

type responseCloseBody struct {
	*io.PipeReader
	mu     sync.Mutex
	closed int
}

func (b *responseCloseBody) Close() error {
	b.mu.Lock()
	b.closed++
	b.mu.Unlock()
	return b.PipeReader.Close()
}

func TestOpenCodeSSEHeartbeatIsTimelyAndCloseUnblocksRead(t *testing.T) {
	r, w := io.Pipe()
	original := &responseCloseBody{PipeReader: r}
	resp := &http.Response{Header: make(http.Header), Body: original}
	go func() { _, _ = io.WriteString(w, ": heartbeat\n") }()
	if err := adaptOpenCodeResponse(resp, responsePlan()); err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	go func() {
		data := make([]byte, 128)
		n, err := resp.Body.Read(data)
		if err != nil {
			result <- "error: " + err.Error()
			return
		}
		result <- string(data[:n])
	}()
	select {
	case got := <-result:
		if got != ": heartbeat\n" {
			t.Fatalf("bad heartbeat: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat waited for another event")
	}
	blocked := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 1)); blocked <- err }()
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("closed body read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the blocked upstream read")
	}
	original.mu.Lock()
	defer original.mu.Unlock()
	if original.closed != 1 {
		t.Fatalf("closed original %d times", original.closed)
	}
	_ = w.Close()
}
