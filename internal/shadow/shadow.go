package shadow

import (
	"log"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
	"github.com/Dimoncelli/wiretuple/internal/gate"
	"github.com/Dimoncelli/wiretuple/internal/proxy"
)

// Metrics is one audit row.
type Metrics struct {
	TS               time.Time
	Path             string
	Mode             string
	TokensBefore     int64
	TokensAfter      int64
	OutputSavingsPct float64
	NetSavingsPct    float64
	LatencyAdded     time.Duration
	Activated        bool
}

// Recorder serializes audit rows, keeps a running summary and persists every
// row to the SQLite file.
type Recorder struct {
	mu        sync.Mutex
	writer    *Writer
	logger    *log.Logger
	rows      int
	activated int
	before    int64
	after     int64
	closed    bool
}

// NewRecorder opens (or creates) the audit database.
func NewRecorder(dbPath string) (*Recorder, error) {
	writer, err := NewWriter(dbPath, defaultPageSize)
	if err != nil {
		return nil, err
	}
	return &Recorder{writer: writer, logger: log.Default()}, nil
}

// NewRecorderWithLogger is NewRecorder with a custom logger.
func NewRecorderWithLogger(dbPath string, logger *log.Logger) (*Recorder, error) {
	recorder, err := NewRecorder(dbPath)
	if err != nil {
		return nil, err
	}
	if logger != nil {
		recorder.logger = logger
	}
	return recorder, nil
}

// maxPathBytes bounds the stored request path so one long URL cannot overflow a
// SQLite page and discard buffered rows.
const maxPathBytes = 256

// truncateUTF8 cuts s to at most limit bytes without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// Record persists one audit row. It is goroutine-safe.
func (r *Recorder) Record(row proxy.ShadowRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}

	activated := 0
	if row.Activated {
		activated = 1
		r.activated++
	}
	mode := row.Mode
	if mode == "" {
		mode = "passthrough"
	}

	err := r.writer.Insert([]Value{
		Text(row.TS.UTC().Format(time.RFC3339Nano)),
		Text(truncateUTF8(row.Path, maxPathBytes)),
		Text(mode),
		Int(row.TokensBefore),
		Int(row.TokensAfter),
		Float(row.OutputSavingsPct),
		Float(row.NetSavingsPct),
		Int(row.LatencyAdded.Microseconds()),
		Int(int64(activated)),
	})
	if err != nil {
		return err
	}

	r.rows++
	r.before += row.TokensBefore
	r.after += row.TokensAfter
	return nil
}

// Summary is the console snapshot printed on Close.
func (r *Recorder) Summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.summaryLocked()
}

func (r *Recorder) summaryLocked() string {
	rate := 0.0
	if r.rows > 0 {
		rate = float64(r.activated) / float64(r.rows) * 100
	}
	outputPct := 0.0
	netPct := 0.0
	if r.before > 0 {
		outputPct = float64(r.before-r.after) / float64(r.before) * 100
		netPct = outputPct
	}
	return formatSummary(r.rows, r.activated, rate, r.before, r.after, outputPct, netPct)
}

func formatSummary(rows, activated int, rate float64, before, after int64, outputPct, netPct float64) string {
	return "wiretuple shadow summary: " +
		"rows=" + itoa(rows) +
		" activated=" + itoa(activated) +
		" activation_rate=" + ftoa(rate) + "%" +
		" tokens_before=" + itoa64(before) +
		" tokens_after=" + itoa64(after) +
		" output_savings=" + ftoa(outputPct) + "%" +
		" net_savings=" + ftoa(netPct) + "%"
}

// Close prints the summary and flushes the database.
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	summary := r.summaryLocked()
	r.mu.Unlock()

	if r.logger != nil {
		r.logger.Print(summary)
	}
	return r.writer.Close()
}

// Simulate evaluates a schema without touching traffic: it runs the gate and the
// compiler and reports what the proxy would have done.
func Simulate(schema *compiler.SchemaNode, legendTokens int64, threshold float64, instances int) Metrics {
	decision := gate.Estimator{ThresholdPct: threshold}.Decide(gate.Input{
		Schema:            schema,
		ExpectedInstances: instances,
		LegendTokens:      legendTokens,
	})

	row := Metrics{
		TokensBefore:     decision.TokensBefore,
		TokensAfter:      decision.TokensAfter,
		OutputSavingsPct: decision.OutputSavingsPct,
		NetSavingsPct:    decision.NetSavingsPct,
		Activated:        decision.Activate,
		Mode:             "passthrough",
	}
	if decision.Activate {
		row.Mode = "activated"
	}
	return row
}

func itoa(v int) string     { return strconv.Itoa(v) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
