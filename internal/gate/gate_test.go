package gate

import (
	"encoding/json"
	"testing"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
)

func schema(t *testing.T, raw string) *compiler.SchemaNode {
	t.Helper()
	var node compiler.SchemaNode
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &node
}

func TestHeavySchemaActivates(t *testing.T) {
	node := schema(t, `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"invoice_identifier": {"type": "string"},
				"customer_reference": {"type": "string"},
				"billing_currency_code": {"type": "string"},
				"subtotal_amount_cents": {"type": "integer"},
				"tax_amount_cents": {"type": "integer"},
				"total_amount_cents": {"type": "integer"}
			}
		}
	}`)

	decision := Estimator{}.Decide(Input{Schema: node, ExpectedInstances: 10, LegendTokens: 12})
	if !decision.Activate {
		t.Fatalf("expected activation, got %+v", decision)
	}
	if decision.NetSavingsPct < 80 {
		t.Fatalf("net savings too low: %v", decision.NetSavingsPct)
	}
	if decision.Reason != "activated" {
		t.Fatalf("reason = %q", decision.Reason)
	}
}

func TestSingleFlatObjectDoesNotActivate(t *testing.T) {
	node := schema(t, `{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`)
	decision := Estimator{}.Decide(Input{Schema: node, ExpectedInstances: 1, LegendTokens: 25})
	if decision.Activate {
		t.Fatalf("single object must not activate: %+v", decision)
	}
	if decision.NetSavingsPct >= 0 {
		t.Fatalf("expected negative net savings, got %v", decision.NetSavingsPct)
	}
}

func TestNetEqualsOutputWithoutLegend(t *testing.T) {
	node := schema(t, `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}}`)
	decision := Estimator{}.Decide(Input{Schema: node, ExpectedInstances: 3, LegendTokens: 0})
	if decision.NetSavingsPct != decision.OutputSavingsPct {
		t.Fatalf("net %v != output %v", decision.NetSavingsPct, decision.OutputSavingsPct)
	}
	if decision.TokensAfter != 0 {
		t.Fatalf("tokens after = %d", decision.TokensAfter)
	}
}

func TestThresholdBoundaryActivatesOnEquality(t *testing.T) {
	node := schema(t, `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}}`)
	// before = 1 instance * 1 token for "a"; legend 0 -> net 100.
	decision := Estimator{ThresholdPct: 100}.Decide(Input{Schema: node, ExpectedInstances: 1, LegendTokens: 0})
	if !decision.Activate {
		t.Fatalf("equality at threshold must activate: %+v", decision)
	}

	above := Estimator{ThresholdPct: 100.5}.Decide(Input{Schema: node, ExpectedInstances: 1, LegendTokens: 0})
	if above.Activate {
		t.Fatalf("threshold above savings must not activate: %+v", above)
	}
}

func TestZeroInstancesAndEmptySchemaAreSafe(t *testing.T) {
	empty := Estimator{}.Decide(Input{Schema: &compiler.SchemaNode{Type: "string"}})
	if empty.Activate || empty.TokensBefore != 0 {
		t.Fatalf("unexpected decision: %+v", empty)
	}

	node := schema(t, `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}}`)
	zero := Estimator{}.Decide(Input{Schema: node, ExpectedInstances: 0, LegendTokens: 0})
	if zero.TokensBefore == 0 {
		t.Fatal("zero instances should fall back to the default estimate")
	}
	if zero.OutputSavingsPct != 100 {
		t.Fatalf("output savings = %v", zero.OutputSavingsPct)
	}
}

func TestInjectedTokenModelIsUsed(t *testing.T) {
	node := schema(t, `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}}`)
	calls := 0
	model := func(any) int64 {
		calls++
		return 10
	}
	decision := Estimator{TokenModel: model}.Decide(Input{Schema: node, ExpectedInstances: 2, LegendTokens: 0})
	if calls == 0 {
		t.Fatal("injected token model was not used")
	}
	if decision.TokensBefore != 20 {
		t.Fatalf("tokens before = %d, want 20", decision.TokensBefore)
	}
}

func TestDefaultTokenModel(t *testing.T) {
	cases := []struct {
		value any
		want  int64
	}{
		{"", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"a-longer-field-name", 5},
		{float64(3.5), 1},
		{true, 1},
		{nil, 1},
	}
	for _, tc := range cases {
		if got := DefaultTokenModel(tc.value); got != tc.want {
			t.Fatalf("DefaultTokenModel(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}
