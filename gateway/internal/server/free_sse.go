package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

// Aggregate the OpenAI SSE envelope only. Raw JSON fields preserve the provider's
// extra response metadata, reasoning formats and precise numeric usage fields.
func aggregateFreeSSE(reader io.Reader, st *chatStat) (map[string]json.RawMessage, error) {
	const maxResponse = 32 << 20
	observer := &freeStreamObserver{reader: io.LimitReader(reader, maxResponse+1), st: st}
	data, err := io.ReadAll(observer)
	if err != nil || len(data) > maxResponse || observer.upstreamError || !observer.validFrame || !observer.terminal {
		return nil, errors.New("incomplete upstream stream")
	}
	result := map[string]json.RawMessage{"object": json.RawMessage(`"chat.completion"`)}
	choices := make(map[int]*freeAggregateChoice)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var frame map[string]json.RawMessage
		if json.Unmarshal(payload, &frame) != nil {
			return nil, errors.New("invalid upstream SSE frame")
		}
		if frame["error"] != nil && !bytes.Equal(frame["error"], []byte("null")) {
			return nil, errors.New("upstream SSE error")
		}
		for key, value := range frame {
			if key != "choices" && key != "object" && key != "error" {
				result[key] = value
			}
		}
		var deltas []map[string]json.RawMessage
		if json.Unmarshal(frame["choices"], &deltas) != nil {
			continue
		}
		for position, delta := range deltas {
			index := position
			_ = json.Unmarshal(delta["index"], &index)
			choice := choices[index]
			if choice == nil {
				choice = &freeAggregateChoice{message: map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`)}, tools: make(map[int]map[string]json.RawMessage)}
				choices[index] = choice
			}
			if finish := delta["finish_reason"]; finish != nil && !bytes.Equal(finish, []byte("null")) {
				choice.finish = finish
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(delta["delta"], &fields) != nil {
				_ = json.Unmarshal(delta["message"], &fields)
			}
			for key, value := range fields {
				switch key {
				case "content", "reasoning", "reasoning_content", "refusal":
					appendRawString(choice.message, key, value)
				case "reasoning_details":
					var old, next []json.RawMessage
					_ = json.Unmarshal(choice.message[key], &old)
					if json.Unmarshal(value, &next) == nil {
						choice.message[key], _ = json.Marshal(append(old, next...))
					} else {
						choice.message[key] = value
					}
				case "tool_calls":
					choice.mergeTools(value)
				default:
					choice.message[key] = value
				}
			}
		}
	}
	if scanner.Err() != nil || len(choices) == 0 {
		return nil, errors.New("invalid upstream SSE stream")
	}
	indices := make([]int, 0, len(choices))
	for index := range choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	out := make([]map[string]json.RawMessage, 0, len(indices))
	for _, index := range indices {
		choice := choices[index]
		if len(choice.tools) > 0 {
			toolIndices := make([]int, 0, len(choice.tools))
			for index := range choice.tools {
				toolIndices = append(toolIndices, index)
			}
			sort.Ints(toolIndices)
			tools := make([]map[string]json.RawMessage, 0, len(toolIndices))
			for _, index := range toolIndices {
				tools = append(tools, choice.tools[index])
			}
			choice.message["tool_calls"], _ = json.Marshal(tools)
		}
		if choice.message["content"] == nil {
			choice.message["content"] = json.RawMessage(`null`)
		}
		row := map[string]json.RawMessage{}
		row["index"], _ = json.Marshal(index)
		row["message"], _ = json.Marshal(choice.message)
		if choice.finish != nil {
			row["finish_reason"] = choice.finish
		} else {
			row["finish_reason"] = json.RawMessage(`"stop"`)
		}
		out = append(out, row)
	}
	result["choices"], _ = json.Marshal(out)
	return result, nil
}

type freeAggregateChoice struct {
	message map[string]json.RawMessage
	tools   map[int]map[string]json.RawMessage
	finish  json.RawMessage
}

func appendRawString(target map[string]json.RawMessage, key string, value json.RawMessage) {
	var next, old string
	if json.Unmarshal(value, &next) != nil {
		if target[key] == nil {
			target[key] = value
		}
		return
	}
	_ = json.Unmarshal(target[key], &old)
	target[key], _ = json.Marshal(old + next)
}

func (c *freeAggregateChoice) mergeTools(value json.RawMessage) {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(value, &tools) != nil {
		return
	}
	for position, delta := range tools {
		index := position
		_ = json.Unmarshal(delta["index"], &index)
		tool := c.tools[index]
		if tool == nil {
			tool = make(map[string]json.RawMessage)
			c.tools[index] = tool
		}
		for key, value := range delta {
			if key == "index" {
				continue
			}
			if key != "function" {
				tool[key] = value
				continue
			}
			var function, change map[string]json.RawMessage
			_ = json.Unmarshal(tool[key], &function)
			if function == nil {
				function = make(map[string]json.RawMessage)
			}
			if json.Unmarshal(value, &change) != nil {
				continue
			}
			for field, fragment := range change {
				if field == "name" || field == "arguments" {
					appendRawString(function, field, fragment)
				} else {
					function[field] = fragment
				}
			}
			tool[key], _ = json.Marshal(function)
		}
	}
}
