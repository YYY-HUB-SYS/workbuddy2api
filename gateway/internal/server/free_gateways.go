package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/freegateway"
)

// Free routes branch before prompt rewriting, session binding and account use.
func (h *Handler) freeChatCompletions(w http.ResponseWriter, r *http.Request, body []byte, model string, stream bool, st *chatStat) {
	resp, err := h.cfg.FreeGateways.Chat(r.Context(), model, body)
	if err != nil {
		st.status = http.StatusBadGateway
		code, message := "free_gateway_unavailable", "free gateway request failed"
		var gatewayError *freegateway.Error
		if errors.As(err, &gatewayError) {
			st.status, code, message = gatewayError.Status, gatewayError.Code, gatewayError.Message
			if gatewayError.RetryAfter != "" {
				w.Header().Set("Retry-After", gatewayError.RetryAfter)
			}
		}
		writeOpenAIError(w, st.status, code, message)
		return
	}
	defer resp.Body.Close()
	ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	reader, sse, sniffErr := sniffFreeBody(resp.Body, ct)
	if sniffErr != nil {
		h.freeResponseError(w, model, st)
		return
	}
	if stream && sse {
		// Allowlist response headers; never copy upstream cookies or hop headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		st.status = resp.StatusCode
		w.WriteHeader(resp.StatusCode)
		observer := &freeStreamObserver{reader: reader, st: st}
		_, copyErr := io.Copy(&flushWriter{w}, observer)
		if copyErr != nil || observer.upstreamError || !observer.terminal || !observer.validFrame {
			h.cfg.FreeGateways.NoteFailure(model)
			st.status = http.StatusBadGateway
			if !observer.upstreamError && r.Context().Err() == nil {
				_, _ = io.WriteString(w, "data: {\"error\":{\"code\":\"free_gateway_stream_interrupted\",\"message\":\"upstream stream was interrupted\"}}\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		} else {
			h.cfg.FreeGateways.NoteSuccess(model)
		}
		return
	}
	if sse {
		result, err := aggregateFreeSSE(reader, st)
		if err != nil {
			h.freeResponseError(w, model, st)
			return
		}
		st.status, st.ttfb = resp.StatusCode, time.Since(st.start)
		h.cfg.FreeGateways.NoteSuccess(model)
		writeJSON(w, resp.StatusCode, result)
		return
	}
	const maxResponse = 32 << 20
	data, err := io.ReadAll(io.LimitReader(reader, maxResponse+1))
	var result map[string]json.RawMessage
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, &result) != nil || result == nil || (result["error"] != nil && !bytes.Equal(result["error"], []byte("null"))) || result["choices"] == nil {
		h.freeResponseError(w, model, st)
		return
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(result["choices"], &choices) != nil || len(choices) == 0 {
		h.freeResponseError(w, model, st)
		return
	}
	st.status, st.ttfb = resp.StatusCode, time.Since(st.start)
	var usage struct {
		CompletionTokens *int `json:"completion_tokens"`
	}
	if json.Unmarshal(result["usage"], &usage) == nil && usage.CompletionTokens != nil {
		st.toks = *usage.CompletionTokens
	}
	if stream {
		// A provider may answer a stream request with one JSON completion.
		// Convert only its envelope to one SSE delta; preserve the message fields.
		for _, choice := range choices {
			if message := choice["message"]; message != nil {
				var delta map[string]json.RawMessage
				if json.Unmarshal(message, &delta) != nil || delta == nil {
					h.freeResponseError(w, model, st)
					return
				}
				var tools []map[string]json.RawMessage
				if json.Unmarshal(delta["tool_calls"], &tools) == nil {
					for index, tool := range tools {
						tool["index"], _ = json.Marshal(index)
					}
					delta["tool_calls"], _ = json.Marshal(tools)
				}
				choice["delta"], _ = json.Marshal(delta)
				delete(choice, "message")
			}
		}
		h.cfg.FreeGateways.NoteSuccess(model)
		result["choices"], _ = json.Marshal(choices)
		result["object"] = json.RawMessage(`"chat.completion.chunk"`)
		chunk, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	h.cfg.FreeGateways.NoteSuccess(model)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (h *Handler) freeResponseError(w http.ResponseWriter, model string, st *chatStat) {
	h.cfg.FreeGateways.NoteFailure(model)
	st.status = http.StatusBadGateway
	writeOpenAIError(w, st.status, "free_gateway_invalid_response", "free gateway returned an empty, interrupted or invalid response")
}

type flushWriter struct{ http.ResponseWriter }

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// Observe bounded SSE lines without changing bytes, comments, tools or reasoning.
type freeStreamObserver struct {
	reader        io.Reader
	st            *chatStat
	line          []byte
	dropping      bool
	upstreamError bool
	validFrame    bool
	terminal      bool
}

func (s *freeStreamObserver) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	for _, c := range p[:n] {
		if c == '\n' {
			if !s.dropping {
				s.observe()
			}
			s.line, s.dropping = s.line[:0], false
		} else if !s.dropping {
			if len(s.line) >= 1<<20 {
				s.line, s.dropping = nil, true
			} else {
				s.line = append(s.line, c)
			}
		}
	}
	if err == io.EOF && !s.dropping && len(s.line) > 0 {
		s.observe()
		s.line = nil
	}
	return n, err
}

func (s *freeStreamObserver) observe() {
	s.st.live.observe(s.line)
	if !bytes.HasPrefix(s.line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(s.line, []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) {
		s.terminal = true
		return
	}
	var frame struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return
	}
	if s.st.ttfb == 0 {
		s.st.ttfb = time.Since(s.st.start)
	}
	if len(frame.Choices) > 0 {
		s.validFrame = true
	}
	for _, choice := range frame.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.terminal = true
		}
	}
	if frame.Error != nil && !bytes.Equal(frame.Error, []byte("null")) {
		s.upstreamError = true
	}
	if frame.Usage != nil && frame.Usage.CompletionTokens != nil {
		s.st.toks = *frame.Usage.CompletionTokens
	}
}

// A bounded sniff respects the actual body even when a proxy labels SSE JSON.
// Read only through the first non-whitespace byte so heartbeats flush promptly.
func sniffFreeBody(body io.Reader, contentType string) (io.Reader, bool, error) {
	br := bufio.NewReader(body)
	var prefix []byte
	for len(prefix) < 512 {
		c, err := br.ReadByte()
		if err != nil {
			return nil, false, err
		}
		prefix = append(prefix, c)
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		sse := c == ':' || c == 'd' || c == 'e' || c == 'i' || c == 'r'
		if c != '{' && c != '[' && !sse && contentType != "text/event-stream" {
			return nil, false, errors.New("unexpected body")
		}
		return io.MultiReader(bytes.NewReader(prefix), br), sse, nil
	}
	return nil, false, errors.New("empty body")
}
