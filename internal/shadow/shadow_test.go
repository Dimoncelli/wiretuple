package shadow

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
	"github.com/Dimoncelli/wiretuple/internal/proxy"
)

func heavySchema(t *testing.T) *compiler.SchemaNode {
	t.Helper()
	var node compiler.SchemaNode
	raw := `{
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
	}`
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &node
}

func TestRecorderPersistsRowsAndSummarizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := NewRecorder(path)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}

	row := proxy.ShadowRecord{
		TS:               time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Path:             "/v1/chat/completions",
		Mode:             "activated",
		TokensBefore:     120,
		TokensAfter:      40,
		OutputSavingsPct: 66.6,
		NetSavingsPct:    60.1,
		LatencyAdded:     150 * time.Microsecond,
		Activated:        true,
	}
	if err := recorder.Record(row); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := recorder.Record(proxy.ShadowRecord{Path: "/v1/messages", Mode: "passthrough", TokensBefore: 10, TokensAfter: 10}); err != nil {
		t.Fatalf("record: %v", err)
	}

	summary := recorder.Summary()
	for _, label := range []string{"tokens_before", "tokens_after", "activation_rate", "output_savings", "net_savings"} {
		if !strings.Contains(summary, label) {
			t.Fatalf("summary missing %q: %s", label, summary)
		}
	}

	if err := recorder.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rows := readDB(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0][2] != "activated" {
		t.Fatalf("mode = %v", rows[0][2])
	}
	if rows[1][3] != int64(10) {
		t.Fatalf("tokens_before = %v", rows[1][3])
	}
}

func TestRecorderIsGoroutineSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	recorder, err := NewRecorder(path)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}

	const workers = 8
	const each = 40
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_ = recorder.Record(proxy.ShadowRecord{
					TS:           time.Now(),
					Path:         "/v1/chat/completions",
					Mode:         "activated",
					TokensBefore: int64(id),
					Activated:    true,
				})
			}
		}(worker)
	}
	wg.Wait()

	if err := recorder.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rows := readDB(t, path)
	if len(rows) != workers*each {
		t.Fatalf("rows = %d, want %d", len(rows), workers*each)
	}
}

func TestRecorderCloseIsIdempotent(t *testing.T) {
	recorder, err := NewRecorder(filepath.Join(t.TempDir(), "once.db"))
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := recorder.Record(proxy.ShadowRecord{}); err != nil {
		t.Fatalf("record after close must be a no-op, got %v", err)
	}
}

func TestSimulateUsesGateAndCompiler(t *testing.T) {
	node := heavySchema(t)
	row := Simulate(node, 12, 5, 10)

	if !row.Activated || row.Mode != "activated" {
		t.Fatalf("expected activation: %+v", row)
	}
	if row.TokensBefore == 0 || row.TokensAfter != 0 {
		t.Fatalf("unexpected tokens: %+v", row)
	}
	if row.NetSavingsPct < 80 {
		t.Fatalf("net savings = %v", row.NetSavingsPct)
	}

	small := &compiler.SchemaNode{Type: "string"}
	off := Simulate(small, 12, 5, 4)
	if off.Activated {
		t.Fatalf("string schema must not activate: %+v", off)
	}
}

func TestRecorderTruncatesLongPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paths.db")
	recorder, err := NewRecorder(path)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}

	long := "https://api.example/v1/chat/completions?trace=" + strings.Repeat("a", 5000)
	unicodePath := strings.Repeat("ñ", 400) // 800 bytes, 2 bytes per rune
	if err := recorder.Record(proxy.ShadowRecord{Path: long, Mode: "activated"}); err != nil {
		t.Fatalf("record long: %v", err)
	}
	if err := recorder.Record(proxy.ShadowRecord{Path: unicodePath, Mode: "activated"}); err != nil {
		t.Fatalf("record unicode: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rows := readDB(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	stored, _ := rows[0][1].(string)
	if len(stored) > 256 {
		t.Fatalf("path not truncated: %d bytes", len(stored))
	}
	unicodeStored, _ := rows[1][1].(string)
	if len(unicodeStored) > 256 {
		t.Fatalf("unicode path not truncated: %d bytes", len(unicodeStored))
	}
	if !utf8.ValidString(unicodeStored) {
		t.Fatal("truncation split a UTF-8 rune")
	}
}
