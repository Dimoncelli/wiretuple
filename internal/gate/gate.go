// Package gate decides whether a schema is worth rewriting. It estimates the
// output tokens saved by positional tuples against the fixed input cost of the
// injected legend, and leaves small payloads untouched.
package gate

import (
	"encoding/json"
	"math"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
)

// DefaultThresholdPct is the break-even threshold used when none is configured.
const DefaultThresholdPct = 5.0

// DefaultInstances is the instance estimate used when the schema does not say.
const DefaultInstances = 4

// Input is one gate evaluation.
type Input struct {
	Schema            *compiler.SchemaNode
	ExpectedInstances int
	LegendTokens      int64
}

// Decision is the gate verdict plus the numbers behind it.
type Decision struct {
	Activate         bool
	Reason           string
	TokensBefore     int64
	TokensAfter      int64
	OutputSavingsPct float64
	NetSavingsPct    float64
}

// Estimator is deterministic and pure. TokenModel is injectable for tests; the
// default counts strings as ceil(bytes/4) tokens (minimum 1) and scalars as 1.
type Estimator struct {
	ThresholdPct float64
	TokenModel   func(any) int64
}

// Decide estimates savings and activates only when NetSavingsPct reaches the
// threshold. Equality activates.
func (e Estimator) Decide(in Input) Decision {
	threshold := e.ThresholdPct
	if threshold <= 0 {
		threshold = DefaultThresholdPct
	}
	model := e.TokenModel
	if model == nil {
		model = DefaultTokenModel
	}

	plan, ok := planFor(in.Schema)
	if !ok || len(plan.TupleKeys) == 0 {
		return Decision{Activate: false, Reason: "not_eligible"}
	}

	instances := in.ExpectedInstances
	if instances <= 0 {
		instances = DefaultInstances
	}

	var before int64
	for _, key := range plan.TupleKeys {
		before += tokensFor(model, key)
	}
	before *= int64(instances)

	after := int64(0)
	legend := in.LegendTokens
	if legend < 0 {
		legend = 0
	}
	if before == 0 {
		return Decision{Activate: false, Reason: "no_savings", TokensBefore: 0, TokensAfter: 0}
	}

	outputPct := percent(before-after, before)
	netPct := percent(before-after-legend, before)

	decision := Decision{
		Activate:         netPct >= threshold,
		Reason:           "below_threshold",
		TokensBefore:     before,
		TokensAfter:      after,
		OutputSavingsPct: outputPct,
		NetSavingsPct:    netPct,
	}
	if decision.Activate {
		decision.Reason = "activated"
	}
	return decision
}

// planFor finds the first tuple-eligible position in the schema.
func planFor(schema *compiler.SchemaNode) (compiler.CompiledPlan, bool) {
	if schema == nil {
		return compiler.CompiledPlan{}, false
	}
	if plan, ok := compiler.Compile(schema); ok {
		return plan, true
	}
	switch schema.Type {
	case "object", "":
		if len(schema.Properties) > 0 {
			return compiler.CompileObject(schema)
		}
	}
	return compiler.CompiledPlan{}, false
}

// DefaultTokenModel is a deterministic, dependency-free token estimator.
func DefaultTokenModel(value any) int64 {
	switch typed := value.(type) {
	case string:
		return tokensForString(typed)
	case json.Number, float64, int, int64, bool:
		return 1
	case nil:
		return 1
	default:
		return 1
	}
}

func tokensFor(model func(any) int64, key string) int64 {
	if count := model(key); count > 0 {
		return count
	}
	return 1
}

func tokensForString(s string) int64 {
	if len(s) == 0 {
		return 1
	}
	count := int64(math.Ceil(float64(len(s)) / 4.0))
	if count < 1 {
		return 1
	}
	return count
}

func percent(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
