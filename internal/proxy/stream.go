package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
	"github.com/Dimoncelli/wiretuple/internal/rehydrator"
)

// accumulator feeds JSON fragments of one field into a rehydrator and reports
// only the bytes produced since the previous call.
type accumulator struct {
	engine  *rehydrator.Rehydrator
	out     *bytes.Buffer
	emitted int
}

func newAccumulator(plan *compiler.RehydrateNode) *accumulator {
	buf := &bytes.Buffer{}
	return &accumulator{engine: rehydrator.New(plan, buf), out: buf}
}

// push returns the newly rehydrated bytes for this fragment (may be empty while
// a tuple element is still incomplete).
func (a *accumulator) push(fragment string) string {
	if fragment == "" {
		return ""
	}
	if _, err := a.engine.Write([]byte(fragment)); err != nil {
		return ""
	}
	return a.tail()
}

func (a *accumulator) tail() string {
	data := a.out.Bytes()
	if len(data) <= a.emitted {
		return ""
	}
	delta := string(data[a.emitted:])
	a.emitted = len(data)
	return delta
}

// streamState keeps one accumulator per streamed field, bound to the schema of
// the tool that produced it.
type streamState struct {
	plan       *requestPlan
	content    map[int]*accumulator
	toolArgs   map[string]*accumulator
	toolNodes  map[string]*compiler.RehydrateNode
	blockText  map[int]*accumulator
	blockInput map[int]*accumulator
	blockNodes map[int]*compiler.RehydrateNode
}

func newStreamState(plan *requestPlan) *streamState {
	return &streamState{
		plan:       plan,
		content:    map[int]*accumulator{},
		toolArgs:   map[string]*accumulator{},
		toolNodes:  map[string]*compiler.RehydrateNode{},
		blockText:  map[int]*accumulator{},
		blockInput: map[int]*accumulator{},
		blockNodes: map[int]*compiler.RehydrateNode{},
	}
}

func (s *streamState) contentFor(index int) *accumulator {
	if s.plan.content == nil {
		return nil
	}
	if acc, ok := s.content[index]; ok {
		return acc
	}
	acc := newAccumulator(s.plan.content)
	s.content[index] = acc
	return acc
}

// toolFor binds a streamed call to its tool schema and returns its accumulator.
func (s *streamState) toolFor(key, name string) *accumulator {
	if acc, ok := s.toolArgs[key]; ok {
		return acc
	}
	tree := s.plan.tool(name)
	if tree == nil {
		return nil
	}
	s.toolNodes[key] = tree
	acc := newAccumulator(tree)
	s.toolArgs[key] = acc
	return acc
}

func (s *streamState) textFor(index int) *accumulator {
	if acc, ok := s.blockText[index]; ok {
		return acc
	}
	tree := s.plan.tool("")
	if tree == nil {
		return nil
	}
	acc := newAccumulator(tree)
	s.blockText[index] = acc
	return acc
}

// inputFor binds one Anthropic tool block to its schema.
func (s *streamState) inputFor(index int, name string) *accumulator {
	if acc, ok := s.blockInput[index]; ok {
		return acc
	}
	tree := s.plan.tool(name)
	if tree == nil {
		return nil
	}
	s.blockNodes[index] = tree
	acc := newAccumulator(tree)
	s.blockInput[index] = acc
	return acc
}

// rewriteEvent rehydrates one SSE JSON event and returns the client payload.
func (s *streamState) rewriteEvent(payload []byte) ([]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return payload, err
	}

	if choices, ok := event["choices"].([]any); ok {
		for position, entry := range choices {
			choice, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			index := position
			if value, ok := choice["index"].(float64); ok {
				index = int(value)
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if content, ok := delta["content"].(string); ok {
				if acc := s.contentFor(index); acc != nil {
					if out := acc.push(content); out != "" {
						delta["content"] = out
					} else {
						delete(delta, "content")
					}
				}
			}
			s.rewriteToolCalls(index, delta)
		}
	}

	if eventType, _ := event["type"].(string); strings.HasPrefix(eventType, "content_block") {
		s.rewriteAnthropicBlock(event)
	}

	return json.Marshal(event)
}

func (s *streamState) rewriteToolCalls(choiceIndex int, delta map[string]any) {
	calls, ok := delta["tool_calls"].([]any)
	if !ok {
		return
	}
	for position, entry := range calls {
		call, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		callIndex := position
		if value, ok := call["index"].(float64); ok {
			callIndex = int(value)
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		key := stringKey(choiceIndex) + ":" + stringKey(callIndex)

		name, _ := function["name"].(string)
		if name != "" {
			if bound, ok := s.toolNodes[key]; ok && bound != s.plan.tool(name) {
				// The name arrived after arguments started: rebind cleanly.
				delete(s.toolArgs, key)
			}
		}
		acc := s.toolFor(key, name)
		if acc == nil {
			delete(function, "arguments")
			continue
		}

		arguments, ok := function["arguments"].(string)
		if !ok {
			continue
		}
		if out := acc.push(arguments); out != "" {
			function["arguments"] = out
		} else {
			delete(function, "arguments")
		}
	}
}

func (s *streamState) rewriteAnthropicBlock(event map[string]any) {
	index := 0
	if value, ok := event["index"].(float64); ok {
		index = int(value)
	}

	// Bind the block to its tool before any delta arrives.
	if block, ok := event["content_block"].(map[string]any); ok {
		if name, ok := block["name"].(string); ok {
			if tree := s.plan.tool(name); tree != nil {
				s.blockNodes[index] = tree
			}
		}
	}

	delta, ok := event["delta"].(map[string]any)
	if !ok {
		return
	}
	if text, ok := delta["text"].(string); ok {
		if acc := s.textFor(index); acc != nil {
			if out := acc.push(text); out != "" {
				delta["text"] = out
			} else {
				delete(delta, "text")
			}
		}
	}
	if partial, ok := delta["partial_json"].(string); ok {
		node, bound := s.blockNodes[index]
		if !bound || node == nil {
			node = s.plan.defaultTool
		}
		if node == nil {
			delete(delta, "partial_json")
			return
		}
		if _, ok := s.blockInput[index]; !ok {
			s.blockInput[index] = newAccumulator(node)
		}
		if acc := s.blockInput[index]; acc != nil {
			if out := acc.push(partial); out != "" {
				delta["partial_json"] = out
			} else {
				delete(delta, "partial_json")
			}
		}
	}
}

// finish flushes every accumulator. The tail is normally already emitted when
// the closing bracket arrived inside the last fragment.
func (s *streamState) finish() {
	for _, acc := range s.content {
		acc.tail()
	}
	for _, acc := range s.toolArgs {
		acc.tail()
	}
	for _, acc := range s.blockText {
		acc.tail()
	}
	for _, acc := range s.blockInput {
		acc.tail()
	}
}

func stringKey(value int) string {
	digits := []byte{}
	if value == 0 {
		return "0"
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
