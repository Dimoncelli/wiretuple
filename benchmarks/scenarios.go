// Package benchmarks defines deterministic, realistic scenarios used by the
// benchmark binary and the Go benchmarks. Every scenario builds its expected
// object payload and the positional tuple payload from the same records, so
// structural equality is guaranteed by construction and still asserted.
package benchmarks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
	"github.com/Dimoncelli/wiretuple/internal/gate"
	"github.com/Dimoncelli/wiretuple/internal/rehydrator"
)

// Scenario is one benchmark case.
type Scenario struct {
	Name     string
	Schema   string
	Records  []map[string]any
	Legend   string
	Expanded []byte
	Tuple    []byte
	Plan     compiler.CompiledPlan
}

// Row is one measured result.
type Row struct {
	Name             string
	TokensBefore     int64
	TokensAfter      int64
	OutputSavingsPct float64
	NetSavingsPct    float64
	P99OverheadMs    float64
	EqualityOK       bool
	OrderOK          bool
}

// DefaultIterations is the number of rehydrations measured per scenario.
const DefaultIterations = 500

// EstimateTokens is a deterministic, dependency-free size model: one token per
// four UTF-8 bytes, minimum one. It is used only for reporting, never for
// correctness.
func EstimateTokens(raw []byte) int64 {
	if len(raw) == 0 {
		return 0
	}
	value := int64(math.Ceil(float64(len(raw)) / 4.0))
	if value < 1 {
		return 1
	}
	return value
}

// Build compiles the scenario schema and renders both payloads. Nested objects
// and nested tuples are encoded recursively from the rehydration plan, so the
// tuple payload always matches what the model would be asked to produce.
func (s Scenario) Build() (Scenario, error) {
	var node compiler.SchemaNode
	if err := json.Unmarshal([]byte(s.Schema), &node); err != nil {
		return s, fmt.Errorf("scenario %s: schema: %w", s.Name, err)
	}
	plan, ok := compiler.Compile(&node)
	if !ok {
		return s, fmt.Errorf("scenario %s: schema is not tuple-eligible", s.Name)
	}

	expected := make([]any, 0, len(s.Records))
	tuples := make([]any, 0, len(s.Records))
	for _, record := range s.Records {
		expected = append(expected, record)
		tuples = append(tuples, encodeTupleElement(plan.Rehydrate, record))
	}

	expanded, err := json.Marshal(expected)
	if err != nil {
		return s, err
	}
	tupleJSON, err := json.Marshal(tuples)
	if err != nil {
		return s, err
	}

	s.Plan = plan
	s.Legend = plan.Legend
	s.Expanded = expanded
	s.Tuple = tupleJSON
	return s, nil
}

// encodeTupleElement encodes one record as the positional array described by a
// tuple-array node.
func encodeTupleElement(node *compiler.RehydrateNode, record any) any {
	if node == nil || node.Kind != compiler.KindTupleArray {
		return encodeWith(node, record)
	}
	fields, ok := record.(map[string]any)
	if !ok {
		return record
	}
	out := make([]any, 0, len(node.Keys))
	for _, key := range node.Keys {
		var child *compiler.RehydrateNode
		if node.Item != nil && node.Item.Kind == compiler.KindObject {
			child = node.Item.Fields[key]
		}
		out = append(out, encodeWith(child, fields[key]))
	}
	return out
}

// encodeWith converts one value into the positional form described by node.
func encodeWith(node *compiler.RehydrateNode, value any) any {
	if node == nil {
		return value
	}
	switch node.Kind {
	case compiler.KindTuple:
		record, ok := value.(map[string]any)
		if !ok {
			return value
		}
		out := make([]any, 0, len(node.Keys))
		for _, key := range node.Keys {
			out = append(out, encodeWith(node.Item.Fields[key], record[key]))
		}
		return out
	case compiler.KindObject:
		record, ok := value.(map[string]any)
		if !ok {
			return value
		}
		out := make(map[string]any, len(record))
		for key, child := range record {
			out[key] = encodeWith(node.Fields[key], child)
		}
		return out
	case compiler.KindArray:
		items, ok := value.([]any)
		if !ok {
			return value
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = encodeWith(node.Item, item)
		}
		return out
	default:
		return value
	}
}

// Run measures one scenario: equality, token savings and rehydration overhead.
func Run(s Scenario, iterations int) (Row, error) {
	built, err := s.Build()
	if err != nil {
		return Row{}, err
	}
	batch := BatchSize
	if iterations > 0 {
		batch = iterations
	}

	transformed, err := rehydrator.Transform(built.Plan.Rehydrate, built.Tuple)
	if err != nil {
		return Row{}, err
	}

	equalityOK, err := structurallyEqual(built.Expanded, transformed)
	if err != nil {
		return Row{}, err
	}
	canonicalOK, err := canonicallyEqual(built.Expanded, transformed)
	if err != nil {
		return Row{}, err
	}
	orderOK := keyOrderMatches(transformed, topLevelKeys(built.Plan))

	before := EstimateTokens(built.Expanded)
	after := EstimateTokens(built.Tuple)
	legend := EstimateTokens([]byte(built.Legend))

	var outputPct, netPct float64
	if before > 0 {
		outputPct = float64(before-after) / float64(before) * 100
		netPct = float64(before-after-legend) / float64(before) * 100
	}

	samples := make([]time.Duration, 0, SampleCount)
	for sample := 0; sample < SampleCount; sample++ {
		start := time.Now()
		for i := 0; i < batch; i++ {
			if _, err := rehydrator.Transform(built.Plan.Rehydrate, built.Tuple); err != nil {
				return Row{}, err
			}
		}
		samples = append(samples, time.Since(start)/time.Duration(batch))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	index := int(math.Min(float64(len(samples)-1), math.Floor(float64(len(samples))*0.99)))

	return Row{
		Name:             built.Name,
		TokensBefore:     before,
		TokensAfter:      after,
		OutputSavingsPct: outputPct,
		NetSavingsPct:    netPct,
		P99OverheadMs:    float64(samples[index].Microseconds()) / 1000,
		EqualityOK:       equalityOK && canonicalOK,
		OrderOK:          orderOK,
	}, nil
}

// topLevelKeys returns the key order of one record (tuple keys for an array).
func topLevelKeys(plan compiler.CompiledPlan) []string {
	if plan.Rehydrate != nil && plan.Rehydrate.Kind == compiler.KindTupleArray {
		return plan.Rehydrate.Keys
	}
	return plan.TupleKeys
}

// canonicallyEqual compares both payloads after a decode/encode round trip, so
// key order and whitespace cannot influence the byte comparison.
func canonicallyEqual(expected, actual []byte) (bool, error) {
	canonical := func(raw []byte) ([]byte, error) {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	left, err := canonical(expected)
	if err != nil {
		return false, err
	}
	right, err := canonical(actual)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func structurallyEqual(expected, actual []byte) (bool, error) {
	var left, right any
	if err := json.Unmarshal(expected, &left); err != nil {
		return false, err
	}
	if err := json.Unmarshal(actual, &right); err != nil {
		return false, err
	}
	return reflect.DeepEqual(left, right), nil
}

// keyOrderMatches verifies that the first rehydrated object lists its keys in
// the legend order.
func keyOrderMatches(transformed []byte, keys []string) bool {
	last := -1
	for _, key := range keys {
		index := strings.Index(string(transformed), `"`+key+`":`)
		if index < 0 || index < last {
			return false
		}
		last = index
	}
	return true
}

// Scenarios returns the three deterministic, privacy-safe benchmark cases.
func Scenarios() []Scenario {
	return []Scenario{
		invoiceScenario(),
		pullRequestScenario(),
		mcpToolsScenario(),
	}
}

func invoiceScenario() Scenario {
	const instances = 40
	records := make([]map[string]any, 0, instances)
	for i := 0; i < instances; i++ {
		records = append(records, map[string]any{
			"line_identifier":     fmt.Sprintf("li_%04d", i),
			"product_sku":         fmt.Sprintf("SKU-%05d", i),
			"product_name":        fmt.Sprintf("Seasonal product %02d for store.example", i),
			"quantity_units":      i%4 + 1,
			"unit_price_cents":    1299 + i*17,
			"billing_currency":    "EUR",
			"tax_rate_percentage": 21,
		})
	}
	return Scenario{
		Name:    "E-commerce invoice line items",
		Records: records,
		Schema: `{
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"line_identifier": {"type": "string"},
					"product_sku": {"type": "string"},
					"product_name": {"type": "string"},
					"quantity_units": {"type": "integer"},
					"unit_price_cents": {"type": "integer"},
					"billing_currency": {"type": "string"},
					"tax_rate_percentage": {"type": "number"}
				}
			}
		}`,
	}
}

func pullRequestScenario() Scenario {
	const instances = 30
	records := make([]map[string]any, 0, instances)
	for i := 0; i < instances; i++ {
		records = append(records, map[string]any{
			"pull_number":    1000 + i,
			"pull_title":     fmt.Sprintf("Refactor module %02d in api.example", i),
			"author_login":   fmt.Sprintf("contributor-%02d", i%7),
			"labels":         []any{"area/api", fmt.Sprintf("size/%d", i%4)},
			"merge_state":    []any{"open", "merged"}[i%2],
			"created_at":     fmt.Sprintf("2026-01-%02dT10:00:00Z", i%28+1),
			"merged_boolean": i%2 == 0,
		})
	}
	return Scenario{
		Name:    "Dev-tool pull request batch",
		Records: records,
		Schema: `{
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"pull_number": {"type": "integer"},
					"pull_title": {"type": "string"},
					"author_login": {"type": "string"},
					"labels": {"type": "array", "items": {"type": "string"}},
					"merge_state": {"type": "string"},
					"created_at": {"type": "string"},
					"merged_boolean": {"type": "boolean"}
				}
			}
		}`,
	}
}

func mcpToolsScenario() Scenario {
	const instances = 25
	records := make([]map[string]any, 0, instances)
	for i := 0; i < instances; i++ {
		records = append(records, map[string]any{
			"server_namespace":     fmt.Sprintf("workspace-%02d", i%5),
			"tool_identifier":      fmt.Sprintf("tool_%03d", i),
			"tool_description":     "Batched invocation payload for dev-tools at api.example",
			"timeout_milliseconds": 5000 + i*250,
			"idempotency_required": i%2 == 0,
			"authorization_scope":  "tools:invoke",
			"retry_max_attempts":   i%3 + 1,
		})
	}
	return Scenario{
		Name:    "MCP tool invocations batch",
		Records: records,
		Schema: `{
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"server_namespace": {"type": "string"},
					"tool_identifier": {"type": "string"},
					"tool_description": {"type": "string"},
					"timeout_milliseconds": {"type": "integer"},
					"idempotency_required": {"type": "boolean"},
					"authorization_scope": {"type": "string"},
					"retry_max_attempts": {"type": "integer"}
				}
			}
		}`,
	}
}

// GateDecision exposes the break-even verdict for a scenario, used in docs and
// in the benchmark table footer.
func GateDecision(s Scenario, threshold float64) gate.Decision {
	var node compiler.SchemaNode
	if err := json.Unmarshal([]byte(s.Schema), &node); err != nil {
		return gate.Decision{Reason: "invalid_schema"}
	}
	return gate.Estimator{ThresholdPct: threshold}.Decide(gate.Input{
		Schema:            &node,
		ExpectedInstances: len(s.Records),
		LegendTokens:      EstimateTokens([]byte("Tuple: [legend]")),
	})
}
