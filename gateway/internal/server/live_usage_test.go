package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestLiveUsageIsReadOnlyAuthenticatedAndContainsNoContent(t *testing.T) {
	h := NewHandler(Config{APIKey: "fixture-secret"})
	for _, key := range []string{"", "wrong", "fixture-secret"} {
		r := httptest.NewRequest("GET", "/v1/usage/live", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if key != "fixture-secret" {
			if w.Code != 401 {
				t.Fatalf("unauthorized status %d", w.Code)
			}
			continue
		}
		var s liveSnapshot
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil || s.Instance == "" {
			t.Fatal(w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("telemetry must not be cached")
		}
	}
	r := h.live.begin()
	r.observe([]byte(`data: {"choices":[{"delta":{"content":"private-text","reasoning_content":"思考"}}]}`))
	b, _ := json.Marshal(h.live.snapshot())
	if strings.Contains(string(b), "private") || strings.Contains(string(b), "思考") {
		t.Fatal("generated content leaked into telemetry")
	}
	r.finish(4)
	r.finish(4)
	if s := h.live.snapshot(); s.Active != 0 || s.Completed != 4 {
		t.Fatalf("completion counted more than once: %+v", s)
	}
}

func TestLiveBodyPreservesFragmentedSSEAndBoundsItsBuffer(t *testing.T) {
	m := newLiveUsage()
	r := m.begin()
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"abcd中文\"}}]}\r\n\r\ndata: [DONE]\n"
	b := &liveBody{ReadCloser: io.NopCloser(strings.NewReader(wire)), request: r}
	var output strings.Builder
	buffer := make([]byte, 1)
	for {
		n, err := b.Read(buffer)
		output.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if output.String() != wire {
		t.Fatal("observer changed forwarded bytes")
	}
	if s := m.snapshot(); s.Estimated != 3 || s.Observed != 3 || s.Active != 1 {
		t.Fatalf("fragmented UTF-8 or SSE counted incorrectly: %+v", s)
	}
	r.finish(-1)
	if s := m.snapshot(); s.Completed != 0 || s.Estimated != 0 || s.Active != 0 {
		t.Fatalf("missing usage must not become invented exact tokens: %+v", s)
	}
	large := &liveBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 2<<20) + "\n")), request: m.begin()}
	_, _ = io.Copy(io.Discard, large)
	if len(large.line) != 0 || cap(large.line) > 1<<20 {
		t.Fatal("oversized frame retained")
	}
}

func TestLiveUsageUpdatesBeforeStreamAndSyncCompletion(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
			})}}
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "fixture", AccessToken: "fixture-token", ExpiresAt: 9999999999}), Upstream: up})
			done := make(chan struct{})
			response := httptest.NewRecorder()
			go func() {
				defer close(done)
				h.ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"cn:hy3","stream":%v,"messages":[]}`, stream))))
			}()
			_, err := io.WriteString(writer, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"abcdabcd\"}}]}\n\n")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for h.live.snapshot().Estimated == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s := h.live.snapshot(); s.Active != 1 || s.Estimated != 2 || s.Completed != 0 {
				t.Fatalf("no in-flight telemetry: %+v", s)
			}
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":7,\"prompt_tokens\":1,\"total_tokens\":8}}\n\ndata: [DONE]\n\n")
			_ = writer.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler failed to finish")
			}
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			if s := h.live.snapshot(); s.Active != 0 || s.Estimated != 0 || s.Completed != 7 {
				t.Fatalf("final usage did not replace the estimate: %+v", s)
			}
		})
	}
}

func TestLiveUsageConcurrentRequestsAndCancellation(t *testing.T) {
	m := newLiveUsage()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := m.begin()
			r.observe([]byte(`data: {"choices":[{"delta":{"reasoning_content":"abcd","tool_calls":[{"function":{"arguments":"1234"}}]}}]}`))
			r.finish(3)
		}()
	}
	wg.Wait()
	cancelled := m.begin()
	cancelled.finish(-1)
	if s := m.snapshot(); s.Active != 0 || s.Completed != 96 || s.Observed != 64 {
		t.Fatalf("concurrent accounting drift: %+v", s)
	}
}
