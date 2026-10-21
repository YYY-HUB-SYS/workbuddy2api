package freegateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
)

// The canonical definitions and v2 wire shape are adapted from dsh-our-free-model
// src/upstream.js at 8b2d8c298759195aa324e64513fdd19fe25881a7 (MIT).
// See the third-party notice shipped with this application.
const openCodeClientUA = "opencode/latest/2.0.26/cli"

// Keep the descriptions and parameter schemas verbatim: the provider checks
// these definitions as part of its free-tier client compatibility contract.
const openCodeReadTool = `{"type":"function","function":{"name":"read","description":"Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"File or directory to read"},"offset":{"type":"integer","minimum":0,"description":"The line or directory entry to start reading from (1-based)"},"limit":{"type":"integer","minimum":0,"description":"The maximum number of lines or directory entries to read (defaults to and capped at 2000)"}},"required":["path"],"additionalProperties":false}}}`
const openCodeShellTool = `{"type":"function","function":{"name":"shell","description":"Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.","parameters":{"type":"object","properties":{"command":{"type":"string","description":"Shell command string to execute"},"workdir":{"type":"string","description":"Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},"timeout":{"type":"integer","minimum":0,"description":"Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},"background":{"type":"boolean","description":"Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."}},"required":["command"],"additionalProperties":false}}}`

// Aliases maps names sent upstream back to the caller's names. Injected marks
// compatibility declarations that the caller cannot execute, even if the caller
// originally declared a conflicting tool with that name.
type openCodePlan struct {
	Aliases  map[string]string
	Declared map[string]bool
	Injected map[string]bool
}

func openCodeHeaders(session string, stream bool) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer public")
	h.Set("User-Agent", openCodeClientUA)
	h.Set("X-Opencode-Client", "cli")
	h.Set("X-Opencode-Session", session)
	h.Set("X-Opencode-Session-Id", session)
	h.Set("X-Session-Affinity", session)
	h.Set("X-Session-Id", session)
	h.Set("X-Opencode-Project", "global")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "*/*")
	}
	return h
}

func rawObject(raw json.RawMessage) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func toolName(raw json.RawMessage) string {
	tool := rawObject(raw)
	return rawString(rawObject(tool["function"])["name"])
}

func renameTool(raw json.RawMessage, name string) json.RawMessage {
	tool := rawObject(raw)
	fn := rawObject(tool["function"])
	fn["name"] = rawJSON(name)
	tool["function"] = rawJSON(fn)
	return rawJSON(tool)
}

func compactEqual(a, b json.RawMessage) bool {
	// Compare JSON definitions structurally: callers may order object keys
	// differently or escape '<'/'&'. UseNumber avoids float64 rounding even in
	// this comparison; the original RawMessage itself is still forwarded.
	canonical := func(raw json.RawMessage) ([]byte, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	left, err := canonical(a)
	if err != nil {
		return false
	}
	right, err := canonical(b)
	return err == nil && bytes.Equal(left, right)
}

func canonicalDefinition(raw, canonical json.RawMessage) bool {
	tool, want := rawObject(raw), rawObject(canonical)
	if rawString(tool["type"]) != "function" {
		return false
	}
	fn, wantFn := rawObject(tool["function"]), rawObject(want["function"])
	return rawString(fn["description"]) == rawString(wantFn["description"]) && compactEqual(fn["parameters"], wantFn["parameters"])
}

func rawArrayField(payload map[string]json.RawMessage, key string) ([]json.RawMessage, error) {
	raw := payload[key]
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, gatewayError(http.StatusBadRequest, "invalid_request", key+" must be an array")
	}
	return items, nil
}

func prepareOpenCode(payload map[string]json.RawMessage) (*openCodePlan, string, error) {
	messages, err := rawArrayField(payload, "messages")
	if err != nil {
		return nil, "", err
	}
	tools, err := rawArrayField(payload, "tools")
	if err != nil {
		return nil, "", err
	}
	// The v2 client always uses SSE upstream. Live controls confirm this free
	// lane rejects stream:false even with the other fingerprint fields correct.
	// The HTTP server keeps the caller's original stream flag and aggregates the
	// SSE response when that caller requested an ordinary JSON completion.
	payload["stream"] = json.RawMessage("true")
	// Derive identity before adding compatibility notes. No prompt or conversation
	// identifiers are saved to disk, retained in a global map, or put in headers.
	session := canonicalOpenCodeSession(openCodeConversationSeed(payload, messages))
	plan := &openCodePlan{Aliases: make(map[string]string), Declared: make(map[string]bool), Injected: make(map[string]bool)}
	taken := make(map[string]bool)
	for _, tool := range tools {
		if name := toolName(tool); name != "" {
			taken[name], plan.Declared[name] = true, true
		}
	}
	seen, present := make(map[string]bool), make(map[string]bool)
	definitions := map[string]json.RawMessage{"read": json.RawMessage(openCodeReadTool), "shell": json.RawMessage(openCodeShellTool)}
	out := make([]json.RawMessage, 0, len(tools)+2)
	for _, tool := range tools {
		name := toolName(tool)
		canonical, reserved := definitions[name]
		if !reserved {
			out = append(out, tool)
			continue
		}
		// Deduplicate only the two exact reserved spellings. Unrelated tools,
		// including Read/Shell and their existing duplicates, remain untouched.
		if seen[name] {
			continue
		}
		seen[name] = true
		if canonicalDefinition(tool, canonical) {
			out = append(out, tool)
			present[name] = true
			continue
		}
		alias := name + "_client"
		for i := 2; taken[alias]; i++ {
			alias = fmt.Sprintf("%s_client%d", name, i)
		}
		taken[alias], plan.Aliases[alias] = true, name
		out = append(out, renameTool(tool, alias))
	}
	var injected []string
	for _, name := range []string{"read", "shell"} {
		if !present[name] {
			out = append(out, definitions[name])
			plan.Injected[name] = true
			injected = append(injected, name)
		}
	}
	payload["tools"] = rawJSON(out)
	aliases := make(map[string]string, len(plan.Aliases))
	for sent, original := range plan.Aliases {
		aliases[original] = sent
	}
	retargetOpenCodeChoice(payload, aliases)
	if len(aliases) > 0 {
		for i, raw := range messages {
			message := rawObject(raw)
			if message == nil {
				continue
			}
			var calls []json.RawMessage
			if json.Unmarshal(message["tool_calls"], &calls) != nil {
				continue
			}
			changed := false
			for n, call := range calls {
				if alias, ok := aliases[toolName(call)]; ok {
					calls[n], changed = renameTool(call, alias), true
				}
			}
			if changed {
				message["tool_calls"] = rawJSON(calls)
				messages[i] = rawJSON(message)
			}
		}
	}
	if len(injected) > 0 {
		note := "Do not call the `read` or `shell` tools; they are placeholders that do nothing. Use the other tools you have been given instead."
		if len(injected) == 1 {
			note = "Do not call the `" + injected[0] + "` tool; it is a placeholder that does nothing. Use the other tools you have been given instead."
		}
		if len(messages) > 0 {
			first := rawObject(messages[0])
			var content string
			if rawString(first["role"]) == "system" && json.Unmarshal(first["content"], &content) == nil {
				first["content"] = rawJSON(content + "\n\n" + note)
				messages[0] = rawJSON(first)
			} else {
				messages = append([]json.RawMessage{rawJSON(map[string]string{"role": "system", "content": note})}, messages...)
			}
		} else {
			messages = []json.RawMessage{rawJSON(map[string]string{"role": "system", "content": note})}
		}
	}
	if len(injected) > 0 || len(aliases) > 0 {
		payload["messages"] = rawJSON(messages)
	}
	return plan, session, nil
}

func retargetOpenCodeChoice(payload map[string]json.RawMessage, aliases map[string]string) {
	choice := payload["tool_choice"]
	object := rawObject(choice)
	if rawString(choice) == "none" || rawString(object["type"]) == "none" {
		delete(payload, "tool_choice")
		return
	}
	if object == nil || len(aliases) == 0 {
		return
	}
	if alias, ok := aliases[rawString(object["name"])]; ok {
		object["name"] = rawJSON(alias)
		payload["tool_choice"] = rawJSON(object)
		return
	}
	fn := rawObject(object["function"])
	if alias, ok := aliases[rawString(fn["name"])]; ok {
		fn["name"] = rawJSON(alias)
		object["function"] = rawJSON(fn)
		payload["tool_choice"] = rawJSON(object)
	}
}

// Explicit conversation IDs take priority. Otherwise pin the session to the
// initial system/developer instructions and first user message, so continuing a
// history or switching model does not mint another session for every turn.
func openCodeConversationSeed(payload map[string]json.RawMessage, messages []json.RawMessage) string {
	keys := []string{"conversation_id", "session_id", "thread_id", "conversationId", "sessionId", "threadId"}
	for _, source := range []map[string]json.RawMessage{payload, rawObject(payload["metadata"])} {
		for _, key := range keys {
			if value := rawString(source[key]); value != "" {
				return "conversation\x00" + value
			}
		}
	}
	var seed struct {
		System []json.RawMessage `json:"system"`
		User   json.RawMessage   `json:"user"`
	}
	for _, raw := range messages {
		message := rawObject(raw)
		switch rawString(message["role"]) {
		case "system", "developer":
			seed.System = append(seed.System, message["content"])
		case "user":
			seed.User = message["content"]
			return string(rawJSON(seed))
		}
	}
	return string(rawJSON(seed))
}

// OpenCode uses a descending 48-bit time encoding with the low twelve bits in
// 0xfc0..0xfff. Hash the seed, then encode it in that layout with counter 1.
// This reproduces the reference canonicalizer without storing a session map.
func canonicalOpenCodeSession(seed string) string {
	digest := sha256.Sum256([]byte("ses\x00" + seed))
	top := uint64(digest[0])<<28 | uint64(digest[1])<<20 | uint64(digest[2])<<12 | uint64(digest[3])<<4 | uint64(digest[4])>>4
	timePart := ^(top<<12 | 1) & 0xffffffffffff
	const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	n := new(big.Int).SetBytes(digest[6:16])
	base, remainder := big.NewInt(62), new(big.Int)
	tail := make([]byte, 14)
	for i := len(tail) - 1; i >= 0; i-- {
		n.QuoRem(n, base, remainder)
		tail[i] = base62[remainder.Int64()]
	}
	return fmt.Sprintf("ses_%012x%s", timePart, tail)
}
