// Command wiretuple-bench prints the deterministic savings table and verifies
// that every rehydrated payload is structurally identical to the original.
package main

import (
	"fmt"
	"os"

	"github.com/Dimoncelli/wiretuple/benchmarks"
)

func main() {
	fmt.Println("wiretuple benchmark — positional tuples vs. repeated JSON keys")
	fmt.Println()

	rows := make([]benchmarks.Row, 0, 3)
	failed := false

	for _, scenario := range benchmarks.Scenarios() {
		row, err := benchmarks.Run(scenario, benchmarks.DefaultIterations)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scenario %q failed: %v\n", scenario.Name, err)
			os.Exit(1)
		}
		rows = append(rows, row)
		if !row.EqualityOK || !row.OrderOK {
			failed = true
		}
	}

	fmt.Println("| Scenario | tokens_before | tokens_after | output_savings_% | net_savings_% | p99_overhead_ms | equality |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---|")
	for _, row := range rows {
		status := "OK"
		if !row.EqualityOK || !row.OrderOK {
			status = "FAIL"
		}
		fmt.Printf("| %s | %d | %d | %.1f%% | %.1f%% | %.3f | %s |\n",
			row.Name, row.TokensBefore, row.TokensAfter, row.OutputSavingsPct, row.NetSavingsPct, row.P99OverheadMs, status)
	}
	fmt.Println()

	for _, row := range rows {
		fmt.Printf("%-34s equality=%v key_order=%v\n", row.Name, row.EqualityOK, row.OrderOK)
	}

	if failed {
		fmt.Fprintln(os.Stderr, "structural equality failed: refusing to report success")
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("all scenarios are bit-for-bit structurally equal after rehydration")
}
