package freegateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	openCodeResponseLimit = 32 << 20
	openCodeEventLimit    = 1 << 20
	openCodeToolLimit     = 256
)

// adaptOpenCodeResponse removes the compatibility-only tools before the normal
// server response observer sees them. RawMessage keeps provider metadata and
// token counts (including integers above 2^53) lossless.
func adaptOpenCodeResponse(resp *http.Response, plan *openCodePlan) error {
	if resp == nil || resp.Body == nil || plan == nil {
		return nil
	}
	original := resp.Body
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	reader := bufio.NewReader(original)
	var prefix []byte
	for len(prefix) < 512 {
		c, err := reader.ReadByte()
		if err != nil {
			return err
		}
		prefix = append(prefix, c)
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		body := io.MultiReader(bytes.NewReader(prefix), reader)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		if c == '{' || c == '[' {
			data, err := io.ReadAll(io.LimitReader(body, openCodeResponseLimit+1))
			if err != nil {
				return err
			}
			if len(data) > openCodeResponseLimit {
				return errors.New("OpenCode response exceeds the response limit")
			}
			data, err = adaptOpenCodeJSON(data, plan)
			if err != nil {
				return err
			}
			resp.Header.Set("Content-Type", "application/json")
			resp.Body = &openCodeBody{Reader: bytes.NewReader(data), original: original}
			return nil
		}
		if c != ':' && c != 'd' && c != 'e' && c != 'i' && c != 'r' {
			return errors.New("OpenCode returned an unexpected response body")
		}
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Body = &openCodeBody{Reader: &openCodeStream{
			reader: bufio.NewReader(body), plan: plan, choices: make(map[string]*openCodeChoice),
		}, original: original}
		return nil
	}
	return errors.New("OpenCode returned an empty response")
}

// Close must still reach the HTTP body, including while a Read is blocked.
type openCodeBody struct {
	io.Reader
	original io.ReadCloser
	once     sync.Once
	err      error
}

func (b *openCodeBody) Close() error {
	b.once.Do(func() { b.err = b.original.Close() })
	return b.err
}

func openCodeHiddenError() *Error {
	return gatewayError(http.StatusBadGateway, "opencode_hidden_tool_response", "OpenCode returned an internal compatibility tool call without an answer; please retry the request")
}

func openCodeHasContent(raw json.RawMessage) bool {
	var content string
	if json.Unmarshal(raw, &content) == nil {
		return strings.TrimSpace(content) != ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return false
	}
	for _, part := range parts {
		var fields map[string]json.RawMessage
		if json.Unmarshal(part, &fields) != nil {
			continue
		}
		for _, field := range []string{"text", "refusal"} {
			if json.Unmarshal(fields[field], &content) == nil && strings.TrimSpace(content) != "" {
				return true
			}
		}
		var image struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(fields["image_url"], &image) == nil && strings.TrimSpace(image.URL) != "" {
			return true
		}
		var audio struct {
			Data       string `json:"data"`
			Transcript string `json:"transcript"`
		}
		if json.Unmarshal(fields["audio"], &audio) == nil && (strings.TrimSpace(audio.Data) != "" || strings.TrimSpace(audio.Transcript) != "") {
			return true
		}
	}
	return false
}

func adaptOpenCodeJSON(data []byte, plan *openCodePlan) ([]byte, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return nil, errors.New("OpenCode returned invalid JSON")
	}
	var choices []map[string]json.RawMessage
	if raw := root["choices"]; raw == nil || json.Unmarshal(raw, &choices) != nil {
		// Provider errors are left intact for the existing error handling.
		return data, nil
	}
	for _, choice := range choices {
		if choice == nil {
			return nil, errors.New("OpenCode returned an invalid choice")
		}
		var message map[string]json.RawMessage
		if json.Unmarshal(choice["message"], &message) != nil || message == nil {
			continue
		}
		var calls []map[string]json.RawMessage
		if raw := message["tool_calls"]; raw == nil || json.Unmarshal(raw, &calls) != nil {
			continue
		}
		kept := make([]map[string]json.RawMessage, 0, len(calls))
		hidden := false
		for _, call := range calls {
			if call == nil {
				return nil, errors.New("OpenCode returned an invalid tool call")
			}
			var function map[string]json.RawMessage
			var name string
			if json.Unmarshal(call["function"], &function) == nil {
				_ = json.Unmarshal(function["name"], &name)
			}
			if plan.Injected[name] {
				hidden = true
				continue
			}
			if original, ok := plan.Aliases[name]; ok {
				function["name"], _ = json.Marshal(original)
				call["function"], _ = json.Marshal(function)
			}
			kept = append(kept, call)
		}
		if hidden && len(kept) == 0 && !openCodeHasContent(message["content"]) {
			return nil, openCodeHiddenError()
		}
		message["tool_calls"], _ = json.Marshal(kept)
		choice["message"], _ = json.Marshal(message)
		if hidden && len(kept) == 0 && bytes.Equal(choice["finish_reason"], []byte(`"tool_calls"`)) {
			choice["finish_reason"] = json.RawMessage(`"stop"`)
		}
	}
	root["choices"], _ = json.Marshal(choices)
	return json.Marshal(root)
}

type openCodeTool struct {
	name, wireName string
	pending        map[string]json.RawMessage
	resolved       bool
	hidden         bool
	index          int
}

type openCodeChoice struct {
	tools   map[string]*openCodeTool
	order   []string
	next    int
	hidden  bool
	content bool
	finish  bool
}

type openCodeStream struct {
	reader   *bufio.Reader
	plan     *openCodePlan
	choices  map[string]*openCodeChoice
	output   []byte
	data     []byte
	preamble []byte
	buffered int
	terminal bool
	done     bool
	err      error
}

func (s *openCodeStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.output) == 0 && s.err == nil && !s.done {
		s.readEvent()
	}
	if len(s.output) > 0 {
		n := copy(p, s.output)
		s.output = s.output[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

func (s *openCodeStream) readEvent() {
	line, err := s.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		var all []byte
		for {
			all = append(all, line...)
			if len(all) > openCodeEventLimit {
				s.err = errors.New("OpenCode SSE line exceeds the response limit")
				return
			}
			line, err = s.reader.ReadSlice('\n')
			if !errors.Is(err, bufio.ErrBufferFull) {
				line = append(all, line...)
				break
			}
		}
	}
	if len(line) > openCodeEventLimit || len(s.data)+len(s.preamble)+len(line) > openCodeEventLimit {
		s.err = errors.New("OpenCode SSE event exceeds the response limit")
		return
	}
	trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	if len(trimmed) == 0 {
		if len(s.data) > 0 {
			s.flushEvent()
		} else if len(s.preamble) > 0 {
			s.output = append(s.output, s.preamble...)
			s.output = append(s.output, '\n')
			s.preamble = nil
		} else if len(line) > 0 {
			s.output = append(s.output, line...)
		}
	} else if bytes.HasPrefix(trimmed, []byte(":")) {
		// Heartbeats cannot wait for the next model token or tool name.
		s.output = append(s.output, line...)
	} else if bytes.HasPrefix(trimmed, []byte("data:")) {
		value := bytes.TrimPrefix(trimmed, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		s.data = append(s.data, value...)
		s.data = append(s.data, '\n')
	} else {
		s.preamble = append(s.preamble, line...)
	}
	if err != nil {
		if err != io.EOF {
			s.err = err
			return
		}
		if len(s.data) > 0 {
			s.flushEvent()
		}
		if s.err == nil && !s.done && (!s.terminal || !s.allChoicesFinished()) {
			s.err = io.ErrUnexpectedEOF
		}
		s.done = true
	}
}

func (s *openCodeStream) allChoicesFinished() bool {
	if len(s.choices) == 0 {
		return false
	}
	for _, choice := range s.choices {
		if !choice.finish {
			return false
		}
	}
	return true
}

// Untrusted SSE indices become long-lived map keys. Keep their representation
// small even when upstream sends a sparse index or malformed huge JSON value.
func openCodeIndex(raw json.RawMessage, fallback int) (string, error) {
	if raw == nil {
		return strconv.Itoa(fallback), nil
	}
	var index int64
	if len(raw) == 0 || len(raw) > 16 || raw[0] < '0' || raw[0] > '9' || json.Unmarshal(raw, &index) != nil || index < 0 || index > 1<<31-1 {
		return "", errors.New("OpenCode returned an invalid choice or tool index")
	}
	return strconv.FormatInt(index, 10), nil
}

func (s *openCodeStream) flushEvent() {
	payload := bytes.TrimSpace(s.data)
	s.data = nil
	if bytes.Equal(payload, []byte("[DONE]")) {
		// A tool name may arrive in fragments right through the final event.
		if err := s.finishPending(); err != nil {
			s.emitError(err)
		}
		s.output = append(s.output, s.preamble...)
		s.output = append(s.output, "data: [DONE]\n\n"...)
		s.preamble = nil
		s.terminal, s.done = true, true
		return
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil || root == nil {
		s.err = errors.New("OpenCode returned an invalid SSE event")
		return
	}
	if err := s.transform(root); err != nil {
		s.err = err
		return
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		s.err = err
		return
	}
	s.output = append(s.output, s.preamble...)
	s.output = append(s.output, "data: "...)
	s.output = append(s.output, encoded...)
	s.output = append(s.output, '\n', '\n')
	s.preamble = nil
}

func (s *openCodeStream) emitError(cause error) {
	code, message := "opencode_invalid_response", "OpenCode returned an incomplete or invalid tool call"
	var gateway *Error
	if errors.As(cause, &gateway) {
		code, message = gateway.Code, gateway.Message
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "type": "upstream_error", "message": message}})
	s.output = append(s.output, "data: "...)
	s.output = append(s.output, payload...)
	s.output = append(s.output, '\n', '\n')
}

func (s *openCodeStream) choice(key string) (*openCodeChoice, error) {
	if choice := s.choices[key]; choice != nil {
		return choice, nil
	}
	if len(s.choices) >= 64 {
		return nil, errors.New("OpenCode returned too many choices")
	}
	choice := &openCodeChoice{tools: make(map[string]*openCodeTool)}
	s.choices[key] = choice
	return choice, nil
}

func (s *openCodeStream) transform(root map[string]json.RawMessage) error {
	var rows []map[string]json.RawMessage
	if raw := root["choices"]; raw == nil || json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	for position, row := range rows {
		if row == nil {
			return errors.New("OpenCode returned an invalid choice")
		}
		key, err := openCodeIndex(row["index"], position)
		if err != nil {
			return err
		}
		choice, err := s.choice(key)
		if err != nil {
			return err
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(row["delta"], &delta) != nil || delta == nil {
			delta = make(map[string]json.RawMessage)
		}
		choice.content = choice.content || openCodeHasContent(delta["content"])
		var calls []map[string]json.RawMessage
		var visible []map[string]json.RawMessage
		if raw := delta["tool_calls"]; raw != nil {
			if json.Unmarshal(raw, &calls) != nil {
				return errors.New("OpenCode returned invalid tool calls")
			}
			for _, call := range calls {
				value, err := s.toolFragment(choice, call)
				if err != nil {
					return err
				}
				if value != nil {
					visible = append(visible, value)
				}
			}
			delete(delta, "tool_calls")
		}
		var finish string
		_ = json.Unmarshal(row["finish_reason"], &finish)
		if finish != "" {
			for _, key := range choice.order {
				tool := choice.tools[key]
				if !tool.resolved {
					value, err := s.resolve(choice, tool, true)
					if err != nil {
						return err
					}
					if value != nil {
						visible = append(visible, value)
					}
				}
			}
			choice.finish, s.terminal = true, true
			if choice.hidden && choice.next == 0 {
				if !choice.content {
					err := openCodeHiddenError()
					root["error"], _ = json.Marshal(map[string]string{"code": err.Code, "type": "upstream_error", "message": err.Message})
					row["finish_reason"] = json.RawMessage("null")
				} else if finish == "tool_calls" {
					row["finish_reason"] = json.RawMessage(`"stop"`)
				}
			}
		}
		if len(visible) > 0 {
			delta["tool_calls"], _ = json.Marshal(visible)
		}
		row["delta"], _ = json.Marshal(delta)
	}
	root["choices"], _ = json.Marshal(rows)
	return nil
}

func (s *openCodeStream) toolFragment(choice *openCodeChoice, fragment map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if fragment == nil {
		return nil, errors.New("OpenCode returned an invalid tool fragment")
	}
	key, err := openCodeIndex(fragment["index"], 0)
	if err != nil {
		return nil, err
	}
	tool := choice.tools[key]
	if tool == nil {
		if len(choice.tools) >= openCodeToolLimit {
			return nil, errors.New("OpenCode returned too many tool calls")
		}
		tool = &openCodeTool{pending: make(map[string]json.RawMessage)}
		choice.tools[key] = tool
		choice.order = append(choice.order, key)
	}
	var function map[string]json.RawMessage
	_ = json.Unmarshal(fragment["function"], &function)
	var name string
	_ = json.Unmarshal(function["name"], &name)
	if tool.resolved {
		if tool.hidden {
			return nil, nil
		}
		if name != "" && tool.wireName != "" {
			// Never leak the tail of an aliased name after restoring it.
			return nil, errors.New("OpenCode changed a resolved tool name")
		}
		fragment["index"], _ = json.Marshal(tool.index)
		return fragment, nil
	}
	tool.name += name
	if len(tool.name) > 1024 {
		return nil, errors.New("OpenCode returned an oversized tool name")
	}
	before, _ := json.Marshal(tool.pending)
	if err := mergeOpenCodeTool(tool.pending, fragment); err != nil {
		return nil, err
	}
	after, _ := json.Marshal(tool.pending)
	s.buffered += len(after) - len(before)
	if s.buffered > openCodeEventLimit {
		return nil, errors.New("OpenCode pending tool calls exceed the response limit")
	}
	return s.resolve(choice, tool, false)
}

func (s *openCodeStream) resolve(choice *openCodeChoice, tool *openCodeTool, final bool) (map[string]json.RawMessage, error) {
	if tool.name == "" {
		if final {
			return nil, errors.New("OpenCode returned a tool call without a name")
		}
		return nil, nil
	}
	_, aliasExact := s.plan.Aliases[tool.name]
	exact := aliasExact || s.plan.Injected[tool.name] || s.plan.Declared[tool.name]
	prefix := false
	for name := range s.plan.Aliases {
		prefix = prefix || name != tool.name && strings.HasPrefix(name, tool.name)
	}
	for name := range s.plan.Injected {
		prefix = prefix || name != tool.name && strings.HasPrefix(name, tool.name)
	}
	for name := range s.plan.Declared {
		prefix = prefix || name != tool.name && strings.HasPrefix(name, tool.name)
	}
	if prefix {
		if !final {
			return nil, nil
		}
		if !exact {
			return nil, errors.New("OpenCode returned an incomplete tool name")
		}
	}
	tool.resolved = true
	pending := tool.pending
	encoded, _ := json.Marshal(pending)
	s.buffered -= len(encoded)
	tool.pending = nil
	if s.plan.Injected[tool.name] {
		tool.hidden, choice.hidden = true, true
		return nil, nil
	}
	name := tool.name
	if original, ok := s.plan.Aliases[name]; ok {
		tool.wireName = name
		name = original
	}
	var function map[string]json.RawMessage
	if json.Unmarshal(pending["function"], &function) != nil || function == nil {
		function = make(map[string]json.RawMessage)
	}
	function["name"], _ = json.Marshal(name)
	pending["function"], _ = json.Marshal(function)
	tool.index = choice.next
	choice.next++
	pending["index"], _ = json.Marshal(tool.index)
	return pending, nil
}

func (s *openCodeStream) finishPending() error {
	var terminalError error
	for key, choice := range s.choices {
		var visible []map[string]json.RawMessage
		for _, index := range choice.order {
			tool := choice.tools[index]
			if !tool.resolved {
				call, err := s.resolve(choice, tool, true)
				if err != nil {
					terminalError = err
					continue
				}
				if call != nil {
					visible = append(visible, call)
				}
			}
		}
		if len(visible) > 0 {
			payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": json.RawMessage(key), "delta": map[string]any{"tool_calls": visible}}}})
			s.output = append(s.output, "data: "...)
			s.output = append(s.output, payload...)
			s.output = append(s.output, '\n', '\n')
		}
		if choice.hidden && choice.next == 0 && !choice.content && !choice.finish {
			terminalError = openCodeHiddenError()
		}
	}
	return terminalError
}

func mergeOpenCodeTool(target, fragment map[string]json.RawMessage) error {
	for key, value := range fragment {
		if key == "function" {
			var previous, next map[string]json.RawMessage
			_ = json.Unmarshal(target[key], &previous)
			if previous == nil {
				previous = make(map[string]json.RawMessage)
			}
			if json.Unmarshal(value, &next) != nil {
				return errors.New("OpenCode returned an invalid tool function")
			}
			for field, raw := range next {
				if field == "name" || field == "arguments" {
					var a, b string
					_ = json.Unmarshal(previous[field], &a)
					if json.Unmarshal(raw, &b) != nil {
						return errors.New("OpenCode returned invalid tool fragments")
					}
					previous[field], _ = json.Marshal(a + b)
				} else {
					previous[field] = raw
				}
			}
			target[key], _ = json.Marshal(previous)
		} else if key == "id" {
			var a, b string
			_ = json.Unmarshal(target[key], &a)
			if json.Unmarshal(value, &b) != nil {
				return errors.New("OpenCode returned an invalid tool id")
			}
			target[key], _ = json.Marshal(a + b)
		} else {
			target[key] = value
		}
	}
	return nil
}
