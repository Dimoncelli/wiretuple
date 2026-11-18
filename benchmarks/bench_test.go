package benchmarks

import (
	"testing"

	"github.com/Dimoncelli/wiretuple/internal/rehydrator"
)

// TestBenchmarkTable prints the same Markdown table as cmd/wiretuple-bench so
// the table can be produced on machines whose execution policy blocks freshly
// built binaries.
func TestBenchmarkTable(t *testing.T) {
	if raceEnabled() {
		previous := SampleCount
		SampleCount = 5
		defer func() { SampleCount = previous }()
	}
	t.Log("| Scenario | tokens_before | tokens_after | output_savings_% | net_savings_% | p99_overhead_ms | equality |")
	t.Log("|---|---:|---:|---:|---:|---:|---|")
	for _, scenario := range Scenarios() {
		row, err := Run(scenario, DefaultIterations)
		if err != nil {
			t.Fatalf("%s: %v", scenario.Name, err)
		}
		status := "OK"
		if !row.EqualityOK || !row.OrderOK {
			status = "FAIL"
			t.Errorf("%s: equality/order check failed", scenario.Name)
		}
		t.Logf("| %s | %d | %d | %.1f%% | %.1f%% | %.3f | %s |",
			row.Name, row.TokensBefore, row.TokensAfter, row.OutputSavingsPct, row.NetSavingsPct, row.P99OverheadMs, status)
	}
}

func TestStructuralEquality(t *testing.T) {
	for _, scenario := range Scenarios() {
		row, err := Run(scenario, 20)
		if err != nil {
			t.Fatalf("%s: %v", scenario.Name, err)
		}
		if !row.EqualityOK {
			t.Fatalf("%s: rehydrated JSON is not structurally equal", scenario.Name)
		}
		if !row.OrderOK {
			t.Fatalf("%s: key order does not match the legend", scenario.Name)
		}
	}
}

func TestSavingsTargets(t *testing.T) {
	// The race detector slows every transform about tenfold, so the sampling
	// window and the latency bound are relaxed only in that build.
	p99Bound := 1.0
	if raceEnabled() {
		previous := SampleCount
		SampleCount = 5
		defer func() { SampleCount = previous }()
		p99Bound = 25.0
	}

	for _, scenario := range Scenarios() {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			row, err := Run(scenario, 20)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if row.OutputSavingsPct < 35 {
				t.Fatalf("output savings %.1f%% below 35%%", row.OutputSavingsPct)
			}
			if !row.EqualityOK || !row.OrderOK {
				t.Fatal("payload is not structurally equal or key order does not match the legend")
			}
			if row.P99OverheadMs >= p99Bound {
				t.Fatalf("p99 overhead %.3f ms exceeds %.1f ms", row.P99OverheadMs, p99Bound)
			}
		})
	}
}

func BenchmarkCompile(b *testing.B) {
	for _, scenario := range Scenarios() {
		scenario := scenario
		b.Run(scenario.Name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := scenario.Build(); err != nil {
					b.Fatalf("build: %v", err)
				}
			}
		})
	}
}

func BenchmarkRehydrate(b *testing.B) {
	for _, scenario := range Scenarios() {
		scenario := scenario
		built, err := scenario.Build()
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		b.Run(scenario.Name, func(b *testing.B) {
			b.SetBytes(int64(len(built.Tuple)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := rehydrator.Transform(built.Plan.Rehydrate, built.Tuple); err != nil {
					b.Fatalf("transform: %v", err)
				}
			}
		})
	}
}
