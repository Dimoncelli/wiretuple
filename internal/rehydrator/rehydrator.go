// Package rehydrator turns positional tuple arrays back into full JSON objects
// incrementally, without regex and without buffering the whole stream.
//
// It is a byte-level scanner: strings, escapes and numbers are recognised so
// that brackets and commas inside string values are never treated as
// structure. Tuple arrays are the only place where bytes are held back, and
// only until the current element ends.
package rehydrator

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
)

type tokenKind int

const (
	tokenWhitespace tokenKind = iota
	tokenPunct
	tokenString
	tokenNumber
	tokenLiteral
)

type token struct {
	kind tokenKind
	raw  []byte
}

const (
	// MaxDepth bounds the container nesting a single document may use.
	MaxDepth = 64
	// MaxTupleCaptureBytes bounds one captured tuple element.
	MaxTupleCaptureBytes = 1 << 20
)

type frameKind int

const (
	frameObject frameKind = iota
	frameArray
	frameTuple
	frameTupleSingle
)

type frameState int

const (
	stateKeyOrEnd frameState = iota
	stateColon
	stateValue
	stateCommaOrEnd
	stateValueOrEnd
)

type frame struct {
	kind  frameKind
	node  *compiler.RehydrateNode
	state frameState
	key   string
	first bool

	// tuple capture
	capturing bool
	overflow  bool
	capture   []byte
	depth     int
}

// Rehydrator is an incremental tuple-to-object transformer.
type Rehydrator struct {
	plan    *compiler.RehydrateNode
	w       io.Writer
	frames  []*frame
	pending []byte
	started bool

	// scan cursor for an unfinished string token (avoids O(n^2) rescans)
	scanPos     int
	scanEscaped bool

	// rawFallback streams bytes unchanged after a depth or capture limit
	rawFallback bool

	violations int
	writeErr   error
}

// New builds a transformer that writes rehydrated JSON to w.
func New(plan *compiler.RehydrateNode, w io.Writer) *Rehydrator {
	return &Rehydrator{plan: plan, w: w}
}

// Reset clears all state so the same Rehydrator can process another stream.
func (r *Rehydrator) Reset() {
	r.frames = nil
	r.pending = nil
	r.started = false
	r.scanPos = 0
	r.scanEscaped = false
	r.rawFallback = false
	r.violations = 0
	r.writeErr = nil
}

// Violations reports how many model shape violations degraded to raw output.
func (r *Rehydrator) Violations() int { return r.violations }

// Write consumes p incrementally and returns the number of input bytes used.
// It only returns an error when the destination writer fails.
func (r *Rehydrator) Write(p []byte) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	r.pending = append(r.pending, p...)
	for {
		tok, size, ok := r.nextToken()
		if !ok {
			break
		}
		r.pending = r.pending[size:]
		if err := r.handle(tok); err != nil {
			r.writeErr = err
			return 0, err
		}
	}
	return len(p), nil
}

// nextToken returns the next complete token, resuming an unfinished string
// scan where the previous call stopped so repeated small writes stay linear.
func (r *Rehydrator) nextToken() (token, int, bool) {
	if len(r.pending) == 0 {
		return token{}, 0, false
	}
	if r.pending[0] != '"' {
		r.scanPos = 0
		r.scanEscaped = false
		return scanToken(r.pending)
	}

	start := r.scanPos
	if start < 1 {
		start = 1
	}
	escaped := r.scanEscaped
	for i := start; i < len(r.pending); i++ {
		b := r.pending[i]
		if escaped {
			escaped = false
			continue
		}
		switch b {
		case '\\':
			escaped = true
		case '"':
			r.scanPos = 0
			r.scanEscaped = false
			return token{kind: tokenString, raw: r.pending[:i+1]}, i + 1, true
		}
	}
	r.scanPos = len(r.pending)
	r.scanEscaped = escaped
	return token{}, 0, false
}

// Flush emits a trailing token (a number or literal at end of stream) and any
// pending whitespace. It never reports a model shape violation as an error.
func (r *Rehydrator) Flush() error {
	if r.writeErr != nil {
		return r.writeErr
	}
	if len(r.pending) > 0 {
		tok, _, _ := scanToken(append(append([]byte(nil), r.pending...), ' '))
		r.pending = nil
		if len(tok.raw) > 0 {
			if err := r.handle(tok); err != nil {
				r.writeErr = err
				return err
			}
		}
	}
	if len(r.frames) > 0 && r.frames[len(r.frames)-1].capturing {
		// A tuple element was cut off: drop it rather than corrupting JSON.
		r.violations++
	}
	r.frames = nil
	r.rawFallback = false
	return nil
}

// Transform rehydrates a complete JSON body.
func Transform(plan *compiler.RehydrateNode, body []byte) ([]byte, error) {
	var buf bytes.Buffer
	r := New(plan, &buf)
	if _, err := r.Write(body); err != nil {
		return nil, err
	}
	if err := r.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// tokenizer
// ---------------------------------------------------------------------------

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isPunct(b byte) bool {
	switch b {
	case '{', '}', '[', ']', ':', ',':
		return true
	}
	return false
}

func isDelimiter(b byte) bool {
	return isSpace(b) || isPunct(b) || b == '"'
}

// scanToken returns the next complete token. ok is false when the token is not
// complete yet and more bytes are needed.
func scanToken(buf []byte) (token, int, bool) {
	if len(buf) == 0 {
		return token{}, 0, false
	}
	switch first := buf[0]; {
	case isSpace(first):
		i := 1
		for i < len(buf) && isSpace(buf[i]) {
			i++
		}
		return token{kind: tokenWhitespace, raw: buf[:i]}, i, true
	case isPunct(first):
		return token{kind: tokenPunct, raw: buf[:1]}, 1, true
	case first == '"':
		escaped := false
		for i := 1; i < len(buf); i++ {
			b := buf[i]
			if escaped {
				escaped = false
				continue
			}
			switch b {
			case '\\':
				escaped = true
			case '"':
				return token{kind: tokenString, raw: buf[:i+1]}, i + 1, true
			}
		}
		return token{}, 0, false
	default:
		i := 0
		for i < len(buf) && !isDelimiter(buf[i]) {
			i++
		}
		if i == 0 {
			// Unknown byte: pass it through on its own.
			return token{kind: tokenLiteral, raw: buf[:1]}, 1, true
		}
		if i == len(buf) {
			return token{}, 0, false
		}
		kind := tokenNumber
		if first == 't' || first == 'f' || first == 'n' {
			kind = tokenLiteral
		}
		return token{kind: kind, raw: buf[:i]}, i, true
	}
}

// ---------------------------------------------------------------------------
// dispatcher
// ---------------------------------------------------------------------------

func (r *Rehydrator) handle(tok token) error {
	if r.rawFallback {
		return r.write(tok.raw)
	}
	if len(r.frames) > 0 && r.frames[len(r.frames)-1].kind == frameTuple {
		return r.handleTuple(tok)
	}
	if len(r.frames) > 0 && r.frames[len(r.frames)-1].kind == frameTupleSingle {
		return r.handleTupleSingle(tok)
	}

	if len(r.frames) == 0 {
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		r.started = true
		return r.dispatchValue(r.plan, tok)
	}

	top := r.frames[len(r.frames)-1]
	if top.kind == frameObject {
		return r.handleObject(top, tok)
	}
	return r.handleArray(top, tok)
}

func (r *Rehydrator) dispatchValue(node *compiler.RehydrateNode, tok token) error {
	switch tok.kind {
	case tokenWhitespace:
		return r.write(tok.raw)
	case tokenPunct:
		switch tok.raw[0] {
		case '{':
			if len(r.frames) >= MaxDepth {
				return r.enterRawFallback(tok.raw)
			}
			r.frames = append(r.frames, &frame{kind: frameObject, node: objectNode(node), state: stateKeyOrEnd})
			return r.write(tok.raw)
		case '[':
			if len(r.frames) >= MaxDepth {
				return r.enterRawFallback(tok.raw)
			}
			if node != nil && node.Kind == compiler.KindTupleArray {
				r.frames = append(r.frames, &frame{kind: frameTuple, node: node, first: true})
				return r.write([]byte("["))
			}
			if node != nil && node.Kind == compiler.KindTuple {
				// One tuple at this position: hold this single record and emit an
				// object when it closes. Bounded to one record, never the stream.
				r.frames = append(r.frames, &frame{kind: frameTupleSingle, node: node, capturing: true, depth: 0})
				return nil
			}
			r.frames = append(r.frames, &frame{kind: frameArray, node: node, state: stateValueOrEnd})
			return r.write(tok.raw)
		}
	}
	return r.write(tok.raw)
}

func (r *Rehydrator) handleObject(top *frame, tok token) error {
	switch top.state {
	case stateKeyOrEnd:
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == '}' {
			return r.pop(tok.raw)
		}
		if tok.kind == tokenString {
			top.key = decodeKey(tok.raw)
			top.state = stateColon
		}
		return r.write(tok.raw)
	case stateColon:
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == ':' {
			top.state = stateValue
		}
		return r.write(tok.raw)
	case stateValue:
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		top.state = stateCommaOrEnd
		var child *compiler.RehydrateNode
		if top.node != nil && top.node.Fields != nil {
			child = top.node.Fields[top.key]
		}
		return r.dispatchValue(child, tok)
	default: // stateCommaOrEnd
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == ',' {
			top.state = stateKeyOrEnd
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == '}' {
			return r.pop(tok.raw)
		}
		return r.write(tok.raw)
	}
}

func (r *Rehydrator) handleArray(top *frame, tok token) error {
	switch top.state {
	case stateValueOrEnd:
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == ']' {
			return r.pop(tok.raw)
		}
		top.state = stateCommaOrEnd
		var child *compiler.RehydrateNode
		if top.node != nil {
			child = top.node.Item
		}
		return r.dispatchValue(child, tok)
	default: // stateCommaOrEnd
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == ',' {
			top.state = stateValueOrEnd
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct && tok.raw[0] == ']' {
			return r.pop(tok.raw)
		}
		return r.write(tok.raw)
	}
}

// handleTuple captures one element at a time and expands it into an object.
func (r *Rehydrator) handleTuple(tok token) error {
	top := r.frames[len(r.frames)-1]

	if !top.capturing {
		if tok.kind == tokenWhitespace {
			return r.write(tok.raw)
		}
		if tok.kind == tokenPunct {
			switch tok.raw[0] {
			case ']':
				return r.pop([]byte("]"))
			case ',':
				return nil
			case '[', '{':
				top.capturing = true
				top.capture = append(top.capture[:0], tok.raw...)
				top.depth = 1
				return nil
			}
		}
		// Scalar where a tuple was expected: keep it raw, never break the stream.
		r.violations++
		return r.writeElementRaw(tok.raw)
	}

	if len(top.capture)+len(tok.raw) > MaxTupleCaptureBytes {
		// Keep memory bounded: flush what we hold and pass the rest through.
		if err := r.writeElementRaw(top.capture); err != nil {
			return err
		}
		top.capture = nil
		top.capturing = false
		top.overflow = true
		return r.enterRawFallback(tok.raw)
	}

	top.capture = append(top.capture, tok.raw...)
	if tok.kind != tokenPunct {
		return nil
	}
	switch tok.raw[0] {
	case '[', '{':
		top.depth++
	case ']', '}':
		top.depth--
		if top.depth == 0 {
			raw := append([]byte(nil), top.capture...)
			top.capture = top.capture[:0]
			top.capturing = false
			if err := r.emitElement(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitElement writes one rehydrated tuple element, or the raw value when the
// model broke the tuple contract.
func (r *Rehydrator) emitElement(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}

	values, ok := splitArray(trimmed)
	if !ok || len(values) != len(r.frames[len(r.frames)-1].node.Keys) {
		r.violations++
		return r.writeElementRaw(trimmed)
	}

	top := r.frames[len(r.frames)-1]
	separator := ""
	if !top.first {
		separator = ","
	}
	top.first = false

	var buf bytes.Buffer
	buf.WriteString(separator)
	buf.WriteByte('{')
	for i, key := range top.node.Keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(encodedKey)
		buf.WriteByte(':')

		var fieldNode *compiler.RehydrateNode
		if top.node.Item != nil && top.node.Item.Kind == compiler.KindObject {
			fieldNode = top.node.Item.Fields[key]
		}
		value := bytes.TrimSpace(values[i])
		if fieldNode != nil && hasTuple(fieldNode) && len(value) > 0 {
			transformed, violations, err := transformWith(fieldNode, value)
			if err != nil {
				return err
			}
			r.violations += violations
			buf.Write(transformed)
		} else {
			buf.Write(value)
		}
	}
	buf.WriteByte('}')
	return r.write(buf.Bytes())
}

// handleTupleSingle buffers one positional tuple and emits it as an object.
func (r *Rehydrator) handleTupleSingle(tok token) error {
	top := r.frames[len(r.frames)-1]
	if len(top.capture)+len(tok.raw) > MaxTupleCaptureBytes {
		// Keep memory bounded: the opening '[' is not part of the capture here.
		if err := r.write(append([]byte("["), top.capture...)); err != nil {
			return err
		}
		top.capture = nil
		return r.enterRawFallback(tok.raw)
	}
	if tok.kind == tokenPunct {
		switch tok.raw[0] {
		case '[', '{':
			top.depth++
		case '}', ']':
			if top.depth == 0 && tok.raw[0] == ']' {
				raw := append([]byte(nil), top.capture...)
				r.frames = r.frames[:len(r.frames)-1]
				return r.emitSingleTuple(top.node, raw)
			}
			top.depth--
		}
	}
	top.capture = append(top.capture, tok.raw...)
	return nil
}

// emitSingleTuple writes {k1:v1,...} for one captured tuple.
func (r *Rehydrator) emitSingleTuple(node *compiler.RehydrateNode, raw []byte) error {
	values, ok := splitArray(append(append([]byte("["), raw...), ']'))
	if !ok || len(values) != len(node.Keys) {
		r.violations++
		out := append([]byte("["), raw...)
		return r.write(append(out, ']'))
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range node.Keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(encodedKey)
		buf.WriteByte(':')
		value := bytes.TrimSpace(values[i])
		var fieldNode *compiler.RehydrateNode
		if node.Item != nil && node.Item.Kind == compiler.KindObject {
			fieldNode = node.Item.Fields[key]
		}
		if fieldNode != nil && hasTuple(fieldNode) && len(value) > 0 {
			transformed, violations, err := transformWith(fieldNode, value)
			if err != nil {
				return err
			}
			r.violations += violations
			buf.Write(transformed)
		} else {
			buf.Write(value)
		}
	}
	buf.WriteByte('}')
	return r.write(buf.Bytes())
}

func (r *Rehydrator) writeElementRaw(raw []byte) error {
	top := r.frames[len(r.frames)-1]
	separator := ""
	if !top.first {
		separator = ","
	}
	top.first = false
	return r.write(append([]byte(separator), raw...))
}

func (r *Rehydrator) pop(closing []byte) error {
	r.frames = r.frames[:len(r.frames)-1]
	return r.write(closing)
}

// enterRawFallback gives up on structure and streams bytes unchanged.
func (r *Rehydrator) enterRawFallback(initial []byte) error {
	r.violations++
	r.rawFallback = true
	r.frames = nil
	return r.write(initial)
}

func (r *Rehydrator) write(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := r.w.Write(b)
	return err
}

func objectNode(node *compiler.RehydrateNode) *compiler.RehydrateNode {
	if node != nil && node.Kind == compiler.KindObject {
		return node
	}
	return nil
}

func decodeKey(raw []byte) string {
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return ""
	}
	return key
}

// splitArray splits a complete JSON array literal into its top-level element
// bytes. String contents are respected.
func splitArray(raw []byte) ([][]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' {
		return nil, false
	}
	var values [][]byte
	depth := 0
	inString := false
	escaped := false
	start := 1
	sawComma := false
	for i := 1; i < len(raw); i++ {
		b := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch b {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth == 1 && i == start {
				_ = i
			}
		case '}', ']':
			if depth == 0 {
				if b != ']' {
					return nil, false
				}
				element := bytes.TrimSpace(raw[start:i])
				if len(element) > 0 || sawComma {
					values = append(values, element)
				}
				return values, true
			}
			depth--
		case ',':
			if depth == 0 {
				values = append(values, bytes.TrimSpace(raw[start:i]))
				start = i + 1
				sawComma = true
			}
		}
	}
	return nil, false
}

func hasTuple(node *compiler.RehydrateNode) bool {
	if node == nil {
		return false
	}
	switch node.Kind {
	case compiler.KindTupleArray, compiler.KindTuple:
		return true
	case compiler.KindObject:
		for _, child := range node.Fields {
			if hasTuple(child) {
				return true
			}
		}
	case compiler.KindArray:
		return hasTuple(node.Item)
	}
	return false
}

func transformWith(node *compiler.RehydrateNode, raw []byte) ([]byte, int, error) {
	var buf bytes.Buffer
	r := New(node, &buf)
	if _, err := r.Write(raw); err != nil {
		return nil, 0, err
	}
	if err := r.Flush(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), r.Violations(), nil
}
