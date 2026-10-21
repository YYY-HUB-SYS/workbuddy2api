// Read-only aggregate telemetry for the desktop HUD. Only in-flight values are
// estimates; completed accounting continues to use upstream usage exclusively.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type liveUsage struct {
	mu        sync.Mutex
	instance  string
	next      uint64
	active    map[uint64]uint64 // weighted character quarters; no text retained
	observed  uint64
	completed uint64
}

type liveRequest struct {
	store *liveUsage
	id    uint64
}

func newLiveUsage() *liveUsage {
	return &liveUsage{instance: fmt.Sprintf("%x", time.Now().UnixNano()), active: make(map[uint64]uint64)}
}

func (m *liveUsage) begin() *liveRequest {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.active[m.next] = 0
	return &liveRequest{m, m.next}
}

func (r *liveRequest) finish(tokens int) {
	if r == nil {
		return
	}
	m := r.store
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.active[r.id]; !ok {
		return
	}
	delete(m.active, r.id)
	if tokens >= 0 {
		m.completed += uint64(tokens)
	}
}

func tokenQuarters(text string) uint64 {
	var count uint64
	for _, c := range text {
		if c <= 0x7f {
			count++
		} else {
			count += 4
		}
	}
	return count
}

func (r *liveRequest) observe(line []byte) {
	if r == nil || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var frame struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				Thinking  string `json:"reasoning"`
				Tools     []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return
	}
	var quarters uint64
	for _, choice := range frame.Choices {
		d := choice.Delta
		quarters += tokenQuarters(d.Content) + tokenQuarters(d.Reasoning) + tokenQuarters(d.Thinking)
		for _, tool := range d.Tools {
			quarters += tokenQuarters(tool.Function.Name) + tokenQuarters(tool.Function.Arguments)
		}
	}
	if quarters == 0 {
		return
	}
	m := r.store
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.active[r.id]; ok {
		m.active[r.id] = old + quarters
		m.observed += quarters
	}
}

type liveSnapshot struct {
	Instance  string  `json:"instance"`
	Active    uint64  `json:"active_requests"`
	Completed uint64  `json:"completed_tokens"`
	Estimated uint64  `json:"active_tokens_estimated"`
	Observed  float64 `json:"observed_tokens_estimated"`
}

func (m *liveUsage) snapshot() liveSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := liveSnapshot{Instance: m.instance, Active: uint64(len(m.active)), Completed: m.completed, Observed: float64(m.observed) / 4}
	var quarters uint64
	for _, n := range m.active {
		quarters += n
	}
	s.Estimated = (quarters + 3) / 4
	return s
}

func (h *Handler) liveStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.live.snapshot())
}

// Observe exactly the bytes consumed by the existing handler. No read-ahead,
// protocol changes, extra goroutines, or generated-content logs.
type liveBody struct {
	io.ReadCloser
	request  *liveRequest
	line     []byte
	dropping bool
}

func (b *liveBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	for _, c := range p[:n] {
		if c == '\n' {
			if !b.dropping {
				b.request.observe(b.line)
			}
			b.line = b.line[:0]
			b.dropping = false
		} else if !b.dropping {
			if len(b.line) >= 1<<20 {
				b.line = nil
				b.dropping = true
			} else {
				b.line = append(b.line, c)
			}
		}
	}
	if err == io.EOF && !b.dropping && len(b.line) > 0 {
		b.request.observe(b.line)
		b.line = nil
	}
	return n, err
}
