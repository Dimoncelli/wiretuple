package rehydrator

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
)

func planFor(t *testing.T, schemaJSON string) *compiler.RehydrateNode {
	t.Helper()
	var node compiler.SchemaNode
	if err := json.Unmarshal([]byte(schemaJSON), &node); err != nil {
		t.Fatalf("schema: %v", err)
	}
	plan, ok := compiler.Compile(&node)
	if !ok {
		t.Fatal("schema did not compile")
	}
	return plan.Rehydrate
}

func titlePlan(t *testing.T) *compiler.RehydrateNode {
	return planFor(t, `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {"a": {"type": "string"}, "b": {"type": "integer"}}
		}
	}`)
}

func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return value
}

func transformAll(t *testing.T, plan *compiler.RehydrateNode, input string, chunk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	r := New(plan, &buf)
	body := []byte(input)
	for i := 0; i < len(body); i += chunk {
		end := i + chunk
		if end > len(body) {
			end = len(body)
		}
		if _, err := r.Write(body[i:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return buf.Bytes()
}

func TestTupleArrayBecomesObjectArray(t *testing.T) {
	plan := titlePlan(t)
	input := `[["alpha", 1], ["beta", 2]]`
	got := transformAll(t, plan, input, 64)

	want := `[{"a":"alpha","b":1},{"a":"beta","b":2}]`
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, []byte(want))) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestAnyChunkBoundaryProducesSameOutput(t *testing.T) {
	plan := titlePlan(t)
	input := `[["alpha", 1], ["beta", 2], ["gamma", -3.5e2]]`
	want := decodeAny(t, transformAll(t, plan, input, len(input)))

	for chunk := 1; chunk <= len(input); chunk++ {
		got := decodeAny(t, transformAll(t, plan, input, chunk))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk %d: got %#v, want %#v", chunk, got, want)
		}
	}
}

func TestUTF8SplitAtEveryByteBoundary(t *testing.T) {
	plan := titlePlan(t)
	// "ñ" is two bytes, "🦌" is four bytes.
	input := `[["cañón 🦌", 7]]`
	want := decodeAny(t, transformAll(t, plan, input, len(input)))

	body := []byte(input)
	for split := 1; split < len(body); split++ {
		var buf bytes.Buffer
		r := New(plan, &buf)
		if _, err := r.Write(body[:split]); err != nil {
			t.Fatalf("split %d first write: %v", split, err)
		}
		if _, err := r.Write(body[split:]); err != nil {
			t.Fatalf("split %d second write: %v", split, err)
		}
		if err := r.Flush(); err != nil {
			t.Fatalf("split %d flush: %v", split, err)
		}
		if got := decodeAny(t, buf.Bytes()); !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: got %#v, want %#v", split, got, want)
		}
	}
}

func TestEscapedQuotesAndBackslashes(t *testing.T) {
	plan := titlePlan(t)
	input := `[["say \"hi\" \\ done", 1]]`
	got := transformAll(t, plan, input, 3)

	decoded := decodeAny(t, got).([]any)
	first := decoded[0].(map[string]any)
	if first["a"] != `say "hi" \ done` {
		t.Fatalf("escaped string mangled: %q", first["a"])
	}
}

func TestBracketsAndCommasInsideStrings(t *testing.T) {
	plan := titlePlan(t)
	input := `[["[1,2]", 3], ["a,b", 4]]`
	got := transformAll(t, plan, input, 5)

	decoded := decodeAny(t, got).([]any)
	if decoded[0].(map[string]any)["a"] != "[1,2]" {
		t.Fatalf("brackets inside string were parsed: %s", got)
	}
	if decoded[1].(map[string]any)["a"] != "a,b" {
		t.Fatalf("comma inside string was parsed: %s", got)
	}
}

func TestNestedTupleInsideOrdinaryArray(t *testing.T) {
	plan := planFor(t, `{
		"type": "object",
		"properties": {
			"groups": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"name": {"type": "string"},
						"rows": {"type": "array", "items": {"type": "object", "properties": {"x": {"type": "integer"}, "y": {"type": "integer"}}}}
					}
				}
			}
		}
	}`)

	input := `{"groups":[["g", [[1,2],[3,4]]]]}`
	got := transformAll(t, plan, input, 4)

	want := `{"groups":[{"name":"g","rows":[{"x":1,"y":2},{"x":3,"y":4}]}]}`
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, []byte(want))) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestOrdinaryArrayIsLeftAlone(t *testing.T) {
	plan := planFor(t, `{
		"type": "object",
		"properties": {
			"tags": {"type": "array", "items": {"type": "string"}},
			"rows": {"type": "array", "items": {"type": "object", "properties": {"x": {"type": "integer"}}}}
		}
	}`)
	input := `{"tags":["a","b"],"rows":[[1],[2]]}`
	got := transformAll(t, plan, input, 2)

	want := `{"tags":["a","b"],"rows":[{"x":1},{"x":2}]}`
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, []byte(want))) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestWrongArityFallsBackToRaw(t *testing.T) {
	plan := titlePlan(t)
	input := `[["only-one"], ["ok", 2]]`

	var buf bytes.Buffer
	r := New(plan, &buf)
	if _, err := r.Write([]byte(input)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if r.Violations() == 0 {
		t.Fatal("expected a violation for wrong arity")
	}

	decoded, ok := decodeAny(t, buf.Bytes()).([]any)
	if !ok || len(decoded) != 2 {
		t.Fatalf("unexpected output: %s", buf.Bytes())
	}
	if _, isObject := decoded[0].(map[string]any); isObject {
		t.Fatalf("wrong-arity tuple must stay raw, got %#v", decoded[0])
	}
	if _, isObject := decoded[1].(map[string]any); !isObject {
		t.Fatalf("valid tuple must rehydrate, got %#v", decoded[1])
	}
}

func TestScalarInsteadOfTupleFallsBack(t *testing.T) {
	plan := titlePlan(t)
	input := `[7, ["ok", 1]]`

	var buf bytes.Buffer
	r := New(plan, &buf)
	if _, err := r.Write([]byte(input)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if r.Violations() == 0 {
		t.Fatal("expected a violation for the scalar element")
	}
	decoded := decodeAny(t, buf.Bytes()).([]any)
	if decoded[0] != float64(7) {
		t.Fatalf("scalar not preserved: %#v", decoded[0])
	}
}

func TestTransformEqualsIncremental(t *testing.T) {
	plan := titlePlan(t)
	input := `[["a", 1], ["b", 2], ["c", 3]]`

	streamed := transformAll(t, plan, input, 3)
	transformed, err := Transform(plan, []byte(input))
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !reflect.DeepEqual(decodeAny(t, streamed), decodeAny(t, transformed)) {
		t.Fatalf("streamed %s != transformed %s", streamed, transformed)
	}
}

func TestNumbersRoundTrip(t *testing.T) {
	plan := planFor(t, `{"type":"array","items":{"type":"object","properties":{"v":{"type":"number"}}}}`)
	input := `[[0],[0.0],[-12.5],[1e3],[-2.5E-4]]`
	got := transformAll(t, plan, input, 7)

	decoded := decodeAny(t, got).([]any)
	values := make([]float64, 0, len(decoded))
	for _, item := range decoded {
		values = append(values, item.(map[string]any)["v"].(float64))
	}
	want := []float64{0, 0, -12.5, 1000, -0.00025}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
}

func TestEmptyTupleArray(t *testing.T) {
	plan := titlePlan(t)
	got := transformAll(t, plan, `[]`, 1)
	if string(bytes.TrimSpace(got)) != "[]" {
		t.Fatalf("empty tuple array = %q", got)
	}
}

func TestRootObjectCompiledAsTuple(t *testing.T) {
	var node compiler.SchemaNode
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`), &node); err != nil {
		t.Fatalf("schema: %v", err)
	}
	plan, ok := compiler.CompileObject(&node)
	if !ok {
		t.Fatal("object did not compile")
	}
	got := transformAll(t, plan.Rehydrate, `["Lisbon", 4]`, 2)
	want := `{"city":"Lisbon","days":4}`
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, []byte(want))) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestResetAndReuse(t *testing.T) {
	plan := titlePlan(t)
	var buf bytes.Buffer
	r := New(plan, &buf)
	if _, err := r.Write([]byte(`[["a", 1]]`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	first := buf.String()

	buf.Reset()
	r.Reset()
	if _, err := r.Write([]byte(`[["b", 2]]`)); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if r.Violations() != 0 {
		t.Fatalf("violations after reset: %d", r.Violations())
	}
	if first == buf.String() {
		t.Fatal("second stream reused stale state")
	}
}

func TestChunkedWritesReturnInputLength(t *testing.T) {
	plan := titlePlan(t)
	var buf bytes.Buffer
	r := New(plan, &buf)
	part := []byte(`[["a", 1]]`)
	n, err := r.Write(part)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(part) {
		t.Fatalf("Write returned %d, want %d", n, len(part))
	}
}

func TestSingleTupleWrongArityKeepsValidJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: `[]`},
		{name: "too few", input: `["Lisbon"]`},
		{name: "too many", input: `["Lisbon", 4, "extra", 9]`},
	}

	var node compiler.SchemaNode
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`), &node); err != nil {
		t.Fatalf("schema: %v", err)
	}
	plan, ok := compiler.CompileObject(&node)
	if !ok {
		t.Fatal("object did not compile")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := New(plan.Rehydrate, &buf)
			if _, err := r.Write([]byte(tc.input)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := r.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if r.Violations() == 0 {
				t.Fatal("expected a violation")
			}
			var decoded any
			if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
				t.Fatalf("fallback is not valid JSON: %q (%v)", buf.String(), err)
			}
			if _, isArray := decoded.([]any); !isArray {
				t.Fatalf("fallback must stay a raw array, got %#v", decoded)
			}
		})
	}
}

func TestMultiByteRunesSplitAtEveryBoundary(t *testing.T) {
	plan := titlePlan(t)
	cases := []struct {
		name  string
		value string
	}{
		{name: "two-byte", value: "café"},
		{name: "three-byte", value: "€ 12"},
		{name: "four-byte", value: "🚀 lanzamiento"},
		{name: "mixed", value: "ñ€🚀 ok"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `[["` + tc.value + `", 1]]`
			want := decodeAny(t, transformAll(t, plan, input, len(input)))
			body := []byte(input)
			for split := 1; split < len(body); split++ {
				var buf bytes.Buffer
				r := New(plan, &buf)
				if _, err := r.Write(body[:split]); err != nil {
					t.Fatalf("split %d first: %v", split, err)
				}
				if _, err := r.Write(body[split:]); err != nil {
					t.Fatalf("split %d second: %v", split, err)
				}
				if err := r.Flush(); err != nil {
					t.Fatalf("split %d flush: %v", split, err)
				}
				if got := decodeAny(t, buf.Bytes()); !reflect.DeepEqual(got, want) {
					t.Fatalf("split %d corrupted the rune: %#v", split, got)
				}
			}
		})
	}
}

func TestEscapedBackslashAndQuoteSequence(t *testing.T) {
	plan := titlePlan(t)
	// JSON source: a literal backslash followed by a quote inside the value.
	input := `[["path C:\\\"quoted\"{x:y}", 1]]`
	got := transformAll(t, plan, input, 3)

	decoded := decodeAny(t, got).([]any)
	value := decoded[0].(map[string]any)["a"].(string)
	if !strings.Contains(value, `\"`) {
		t.Fatalf("escaped sequence mangled: %q", value)
	}
	if !strings.Contains(value, "{x:y}") {
		t.Fatalf("braces inside a string were parsed: %q", value)
	}
}

func TestNestedKindTupleInsideTupleArray(t *testing.T) {
	plan := planFor(t, `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"address": {"type": "object", "properties": {"city": {"type": "string"}, "zip": {"type": "string"}}}
			}
		}
	}`)

	input := `[[["Madrid", "28001"], "a1"]]`
	got := transformAll(t, plan, input, 4)
	want := `[{"address":{"city":"Madrid","zip":"28001"},"id":"a1"}]`
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, []byte(want))) {
		t.Fatalf("got %s, want %s", got, want)
	}

	// Wrong arity in the nested tuple must fall back to a valid raw array.
	var buf bytes.Buffer
	r := New(plan, &buf)
	if _, err := r.Write([]byte(`[[["Madrid"], "a1"]]`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if r.Violations() == 0 {
		t.Fatal("expected a nested arity violation")
	}
	var decoded any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("fallback invalid: %q", buf.String())
	}
	nested := decoded.([]any)[0].(map[string]any)["address"]
	if _, isArray := nested.([]any); !isArray {
		t.Fatalf("nested fallback must stay an array, got %#v", nested)
	}
}

func TestLongStringInSingleByteWritesIsLinear(t *testing.T) {
	plan := titlePlan(t)
	value := strings.Repeat("a", 200_000)
	input := `[["` + value + `", 1]]`
	body := []byte(input)

	var buf bytes.Buffer
	r := New(plan, &buf)
	start := time.Now()
	for _, b := range body {
		if _, err := r.Write([]byte{b}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("1-byte writes took %v, expected linear time under 500ms", elapsed)
	}
	if !strings.Contains(buf.String(), value) {
		t.Fatal("long string was corrupted")
	}
}

func TestBracketFloodIsBounded(t *testing.T) {
	plan := titlePlan(t)
	flood := strings.Repeat("[", 500*1024)

	var buf bytes.Buffer
	r := New(plan, &buf)
	if _, err := r.Write([]byte(flood)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if r.Violations() == 0 {
		t.Fatal("expected a depth violation")
	}
	if buf.Len() > len(flood)+64 {
		t.Fatalf("output grew beyond the input: %d > %d", buf.Len(), len(flood))
	}
}

func TestTupleOverflowStaysValidJSON(t *testing.T) {
	plan := planFor(t, `{
		"type": "object",
		"properties": {"items": {"type": "array", "items": {
			"type": "object",
			"properties": {"id": {"type": "integer"}, "text": {"type": "string"}}
		}}},
		"required": ["items"]
	}`)

	// element builds a tuple of exactly size bytes: [1,"<pad>"].
	element := func(size int) string {
		return `[1,"` + strings.Repeat("x", size-len(`[1,""]`)) + `"]`
	}
	run := func(input string) ([]byte, int) {
		var buf bytes.Buffer
		r := New(plan, &buf)
		if _, err := r.Write([]byte(input)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := r.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		return buf.Bytes(), r.Violations()
	}

	cases := []struct {
		name       string
		input      string
		violations int
	}{
		{"exact limit first", `{"items":[` + element(MaxTupleCaptureBytes) + `]}`, 0},
		{"exact limit second", `{"items":[[2,"a"],` + element(MaxTupleCaptureBytes) + `]}`, 0},
		{"over limit first", `{"items":[` + element(MaxTupleCaptureBytes+1) + `]}`, 1},
		{"over limit second", `{"items":[[2,"a"],` + element(MaxTupleCaptureBytes+1) + `]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, violations := run(tc.input)
			if !json.Valid(out) {
				t.Fatalf("output is not valid JSON (len=%d, head=%.80q)", len(out), out)
			}
			if violations != tc.violations {
				t.Fatalf("violations = %d, want %d", violations, tc.violations)
			}
		})
	}

	t.Run("single tuple over limit", func(t *testing.T) {
		var node compiler.SchemaNode
		if err := json.Unmarshal([]byte(`{"type":"object","properties":{"id":{"type":"integer"},"text":{"type":"string"}}}`), &node); err != nil {
			t.Fatalf("schema: %v", err)
		}
		single, ok := compiler.CompileObject(&node)
		if !ok {
			t.Fatal("object did not compile")
		}
		var buf bytes.Buffer
		r := New(single.Rehydrate, &buf)
		// The opening '[' is not part of the single-tuple capture, so go a few bytes past the limit.
		if _, err := r.Write([]byte(element(MaxTupleCaptureBytes + 3))); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := r.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if !json.Valid(buf.Bytes()) {
			t.Fatalf("output is not valid JSON (len=%d)", buf.Len())
		}
		if r.Violations() != 1 {
			t.Fatalf("violations = %d, want 1", r.Violations())
		}
	})
}
