package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// Real HTTP-level failover must keep the model, messages, tools and generation
// parameters, including when the healthy account lies beyond the old three tries.
func TestRateAndUnavailableFailoverKeepsSameModelAndContext(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("status%d/stream%t", status, stream), func(t *testing.T) {
				var accounts []*auth.Auth
				for i := 0; i < 7; i++ {
					accounts = append(accounts, &auth.Auth{
						UID: fmt.Sprintf("u%d", i), AccessToken: fmt.Sprintf("token%d", i),
						ExpiresAt: 9999999999,
					})
				}
				p := testPoolWith(accounts...)
				for i, a := range accounts {
					p.SetCredits(a.UID, int64(7000-i*1000))
				}
				body := fmt.Sprintf(`{"model":"cn:hy3","stream":%t,"temperature":0.2,"max_tokens":321,"messages":[{"role":"user","content":"remember alpha"},{"role":"assistant","content":"alpha"},{"role":"user","content":"repeat it"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, stream)
				var want map[string]any
				if err := json.Unmarshal([]byte(body), &want); err != nil {
					t.Fatal(err)
				}
				want["model"] = "hy3" // Only the gateway's routing prefix is stripped.
				seen := map[string]bool{}
				up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{
					Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						token := r.Header.Get("Authorization")
						if seen[token] {
							t.Fatalf("same failed account retried: %s", token)
						}
						seen[token] = true
						var got map[string]any
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Fatal(err)
						}
						for _, key := range []string{"model", "messages", "tools", "temperature", "max_tokens"} {
							if !reflect.DeepEqual(got[key], want[key]) {
								t.Fatalf("%s changed during rotation: got=%v want=%v", key, got[key], want[key])
							}
						}
						code, payload, contentType := status, `{"message":"temporarily unavailable"}`, "application/json"
						if token == "Bearer token6" {
							code, payload, contentType = 200, strings.ReplaceAll(sseOK, "glm-5.2", "hy3"), "text/event-stream"
						}
						return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
					}),
				}}
				h := NewHandler(Config{Pool: p, Upstream: up})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
				if rec.Code != 200 || len(seen) != len(accounts) {
					t.Fatalf("failover stopped early: status=%d attempts=%d body=%s", rec.Code, len(seen), rec.Body)
				}
				if strings.Contains(rec.Body.String(), `"error"`) {
					t.Fatalf("earlier failure leaked into successful output: %s", rec.Body)
				}
			})
		}
	}
}

func TestExplicitFailoverLimitIsRespected(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999},
	)
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 503, `{"message":"unavailable"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"hy3","messages":[]}`)))
	if calls != 1 || rec.Code != 503 {
		t.Fatalf("explicit limit ignored: calls=%d status=%d", calls, rec.Code)
	}
}
