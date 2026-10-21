package freegateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func TestCatalogFiltersPaidAndUnsupportedProtocols(t *testing.T) {
	var count atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if strings.HasPrefix(r.URL.Path, "/opencode/") {
			if r.Header.Get("Authorization") != "Bearer public" {
				t.Error("OpenCode missing public bearer")
			}
			io.WriteString(w, `{"data":[{"id":"paid-model"},{"id":"big-pickle"},{"id":"beam-free"},{"id":"beam-free"},{"id":"muse-spark-free"},{"id":"union-alpha"}]}`)
		} else {
			if r.Header.Get("Authorization") != "" {
				t.Error("Kilo must be keyless")
			}
			io.WriteString(w, `{"data":[{"id":"vendor/paid:free","isFree":false},{"id":"vendor/keyless","isFree":true,"context_length":8192},{"id":"unspecified"}]}`)
		}
	}))
	defer ts.Close()
	m := New(Config{}, Options{OpenCodeBase: ts.URL + "/opencode", KiloBase: ts.URL + "/kilo"})
	models := m.Models(context.Background())
	if len(models) != 3 || models[0].ID != "kilo:vendor/keyless" || models[1].ID != "opencode:beam-free" || models[2].ID != "opencode:big-pickle" {
		t.Fatalf("unexpected free catalog: %+v", models)
	}
	_ = m.Models(context.Background())
	if count.Load() != 2 {
		t.Fatalf("cache missed: %d", count.Load())
	}
	status := m.Status()
	if status[0].Available || status[0].InferenceVerified || !status[0].CatalogAvailable || status[0].ModelCount != 2 || status[1].ModelCount != 1 {
		t.Fatalf("bad status: %+v", status)
	}
}

func TestCatalogSingleFlightAndIndependentFailures(t *testing.T) {
	var openCalls, kiloCalls atomic.Int32
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/opencode/") {
			openCalls.Add(1)
			w.WriteHeader(503)
			return
		}
		kiloCalls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
	}))
	defer ts.Close()
	m := New(Config{}, Options{OpenCodeBase: ts.URL + "/opencode", KiloBase: ts.URL + "/kilo"})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if models := m.Models(context.Background()); len(models) != 1 {
				t.Errorf("failure erased healthy provider: %+v", models)
			}
		}()
	}
	<-entered
	close(gate)
	wg.Wait()
	if openCalls.Load() != 1 || kiloCalls.Load() != 1 {
		t.Fatalf("not single flight / backoff: %d/%d", openCalls.Load(), kiloCalls.Load())
	}
	s := m.Status()
	if s[0].LastError == "" || s[0].CatalogAvailable || !s[1].CatalogAvailable {
		t.Fatalf("independent status missing: %+v", s)
	}
}

func TestExpiredCatalogFailureIsFailClosed(t *testing.T) {
	var fail atomic.Bool
	var posts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			io.WriteString(w, `{}`)
			return
		}
		if fail.Load() {
			w.WriteHeader(502)
			return
		}
		io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
	}))
	defer ts.Close()
	m := New(Config{OpenCode: ProviderConfig{Enabled: boolPtr(false)}}, Options{KiloBase: ts.URL})
	if len(m.Models(context.Background())) != 1 {
		t.Fatal("initial catalog missing")
	}
	m.providers[1].mu.Lock()
	m.providers[1].fetched = time.Now().Add(-time.Hour)
	m.providers[1].mu.Unlock()
	fail.Store(true)
	_, err := m.Chat(context.Background(), "kilo:north:free", []byte(`{"model":"kilo:north:free"}`))
	if err == nil || posts.Load() != 0 {
		t.Fatal("expired free model was forwarded after failed refresh")
	}
	if models := m.Models(context.Background()); len(models) != 0 {
		t.Fatalf("stale roster leaked: %+v", models)
	}
}

func TestChatPreservesPayloadAndRejectsUnlistedModels(t *testing.T) {
	var posts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		posts.Add(1)
		data, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Error(err)
		}
		if string(payload["model"]) != `"north:free"` || string(payload["large"]) != `9007199254740993` || string(payload["reasoning"]) != `{"enabled":false}` || string(payload["tools"]) != `[]` || string(payload["tool_choice"]) != `"none"` {
			t.Errorf("payload lost or injected values: %s", data)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Device-Token") != "" {
			t.Error("unexpected credentials")
		}
		io.WriteString(w, `{"choices":[],"usage":{"completion_tokens":3}}`)
	}))
	defer ts.Close()
	m := New(Config{OpenCode: ProviderConfig{Enabled: boolPtr(false)}}, Options{KiloBase: ts.URL})
	for _, model := range []string{"kilo:paid", "kilo:invented:free", "opencode:beam-free"} {
		if _, err := m.Chat(context.Background(), model, []byte(`{"model":"x"}`)); err == nil {
			t.Fatalf("forwarded forbidden model %s", model)
		}
	}
	resp, err := m.Chat(context.Background(), "kilo:north:free", []byte(`{"model":"kilo:north:free","large":9007199254740993,"reasoning":{"enabled":false},"tools":[],"tool_choice":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if posts.Load() != 1 {
		t.Fatalf("unexpected post count %d", posts.Load())
	}
}

func TestOpenCodeRestrictionIsSafeAndObservable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"big-pickle"}]}`)
			return
		}
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"name":"FreeTierError","message":"secret https://internal.test?key=private"}}`)
	}))
	defer ts.Close()
	m := New(Config{Kilo: ProviderConfig{Enabled: boolPtr(false)}}, Options{OpenCodeBase: ts.URL})
	_, err := m.Chat(context.Background(), "opencode:big-pickle", []byte(`{"model":"opencode:big-pickle"}`))
	var gatewayError *Error
	if !errors.As(err, &gatewayError) || gatewayError.Status != 403 || gatewayError.Code != "free_tier_restricted" || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe or missing error: %v", err)
	}
	status := m.Status()[0]
	if status.LastError == "" || status.Available {
		t.Fatalf("restriction hidden: %+v", status)
	}
}

func TestChatCancellationAndTotalTimeout(t *testing.T) {
	ended := make(chan struct{}, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"north:free","isFree":true}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": KILO PROCESSING\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		ended <- struct{}{}
	}))
	defer ts.Close()
	m := New(Config{OpenCode: ProviderConfig{Enabled: boolPtr(false)}}, Options{KiloBase: ts.URL, ChatTimeout: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := m.Chat(ctx, "kilo:north:free", []byte(`{"model":"kilo:north:free","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("cancel did not stop body")
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("upstream context did not cancel")
	}
	resp, err = m.Chat(context.Background(), "kilo:north:free", []byte(`{"model":"kilo:north:free","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("total timeout did not stop stalled body")
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("timed out upstream did not cancel")
	}
}

func TestConfigZeroDefaultsAndExplicitFalse(t *testing.T) {
	for _, raw := range []string{`{}`, `{"opencode":{},"kilo":{}}`} {
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.OpenCode.IsEnabled() || !cfg.Kilo.IsEnabled() {
			t.Fatal("missing enabled should default true")
		}
	}
	var cfg Config
	json.Unmarshal([]byte(`{"opencode":{"enabled":false},"kilo":{"enabled":false}}`), &cfg)
	if New(cfg).AnyEnabled() {
		t.Fatal("explicit disable ignored")
	}
}
