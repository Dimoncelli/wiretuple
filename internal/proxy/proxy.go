// Package proxy implements the wiretuple HTTP reverse proxy. It rewrites
// structured-output and function-calling schemas into positional tuples on the
// request path, and rehydrates tuple responses back into objects.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Dimoncelli/wiretuple/internal/compiler"
	"github.com/Dimoncelli/wiretuple/internal/gate"
	"github.com/Dimoncelli/wiretuple/internal/rehydrator"
)

// DefaultMaxBodyBytes caps the request body read from clients.
const DefaultMaxBodyBytes = 4 << 20

// ShadowRecord is one passive-audit row. It lives here so the proxy can report
// through an interface without importing the shadow package.
type ShadowRecord struct {
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

// ShadowRecorder receives simulated audit rows. A nil recorder disables audit.
type ShadowRecorder interface {
	Record(ShadowRecord) error
	Close() error
}

// Config configures the proxy.
type Config struct {
	Upstream     string
	BreakEvenPct float64
	LegendTokens int64
	Activate     bool
	Shadow       ShadowRecorder
	MaxBodyBytes int64
	Logger       *log.Logger
}

// requestPlan routes each part of a response to the schema that produced it.
// Multiple tools must never share one rehydration tree.
type requestPlan struct {
	content     *compiler.RehydrateNode
	tools       map[string]*compiler.RehydrateNode
	defaultTool *compiler.RehydrateNode
}

func newRequestPlan() *requestPlan {
	return &requestPlan{tools: make(map[string]*compiler.RehydrateNode)}
}

func (p *requestPlan) addTool(name string, tree *compiler.RehydrateNode) {
	if tree == nil {
		return
	}
	if name != "" {
		p.tools[name] = tree
	}
	if p.defaultTool == nil {
		p.defaultTool = tree
	}
}

// tool resolves the tree for a named tool, falling back to the first one.
func (p *requestPlan) tool(name string) *compiler.RehydrateNode {
	if p == nil {
		return nil
	}
	if name != "" {
		if tree, ok := p.tools[name]; ok {
			return tree
		}
	}
	return p.defaultTool
}

func (p *requestPlan) empty() bool {
	return p == nil || (p.content == nil && p.defaultTool == nil)
}

// Proxy is the reverse proxy handler.
type Proxy struct {
	cfg      Config
	upstream *url.URL
	client   *http.Client
	gate     gate.Estimator
}

// New validates the configuration and builds a proxy.
func New(cfg Config) (*Proxy, error) {
	if strings.TrimSpace(cfg.Upstream) == "" {
		return nil, errors.New("proxy: upstream URL is required")
	}
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil || upstream.Host == "" {
		return nil, errors.New("proxy: invalid upstream URL")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, errors.New("proxy: upstream must be http or https")
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Proxy{
		cfg:      cfg,
		upstream: upstream,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		gate: gate.Estimator{ThresholdPct: cfg.BreakEvenPct},
	}, nil
}

// ServeHTTP forwards the request upstream, rewriting schemas when worthwhile.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	body, err := readBounded(r.Body, p.cfg.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}

	rewrite, plan := p.prepareRequest(r.URL.Path, body, r)

	target := *p.upstream
	target.Path = singleJoin(p.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	var reader io.Reader
	if rewrite != nil {
		reader = bytes.NewReader(rewrite)
	} else {
		reader = bytes.NewReader(body)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), reader)
	if err != nil {
		http.Error(w, "cannot build upstream request", http.StatusBadGateway)
		return
	}
	copyHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Header.Set("Accept-Encoding", "identity")
	if rewrite != nil {
		upstreamReq.ContentLength = int64(len(rewrite))
	}

	resp, err := p.client.Do(upstreamReq)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responsePlan := plan
	streaming := plan != nil && !plan.empty() && (isEventStream(resp.Header.Get("Content-Type")) || requestStreams(body))

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.Header().Del("Transfer-Encoding")

	if plan == nil || plan.empty() {
		w.WriteHeader(resp.StatusCode)
		_ = copyBody(w, resp.Body, r.Context())
		p.record(r.URL.Path, nil, started)
		return
	}

	if streaming {
		w.WriteHeader(resp.StatusCode)
		flusher, _ := w.(http.Flusher)
		if err := p.pipeSSE(w, resp.Body, responsePlan, r.Context(), flusher); err != nil && !errors.Is(err, context.Canceled) {
			p.cfg.Logger.Printf("wiretuple: sse stream ended: %v", err)
		}
		p.record(r.URL.Path, responsePlan, started)
		return
	}

	payload, err := readBounded(resp.Body, maxResponseBytes(p.cfg.MaxBodyBytes))
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "upstream response too large", http.StatusBadGateway)
			return
		}
		http.Error(w, "cannot read upstream response", http.StatusBadGateway)
		return
	}
	transformed, err := p.transformJSON(r.URL.Path, payload, responsePlan)
	if err != nil {
		transformed = payload
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(transformed)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(transformed)
	p.record(r.URL.Path, responsePlan, started)
}

// prepareRequest returns the rewritten body (nil when untouched) and the plan
// used to rehydrate each part of the response.
func (p *Proxy) prepareRequest(path string, body []byte, _ *http.Request) ([]byte, *requestPlan) {
	if !p.cfg.Activate || len(body) == 0 {
		return nil, nil
	}

	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, nil
	}

	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return p.prepareOpenAI(document)
	case strings.HasSuffix(path, "/v1/messages"):
		return p.prepareAnthropic(document)
	default:
		return nil, nil
	}
}

func (p *Proxy) prepareOpenAI(document map[string]any) ([]byte, *requestPlan) {
	plan := newRequestPlan()
	changed := false

	if format, ok := document["response_format"].(map[string]any); ok {
		if jsonSchema, ok := format["json_schema"].(map[string]any); ok {
			if schema, ok := jsonSchema["schema"].(map[string]any); ok {
				if rewritten, tree, ok := p.rewriteSchema(schema); ok {
					jsonSchema["schema"] = rewritten
					plan.content = tree
					changed = true
				}
			}
		}
	}

	if tools, ok := document["tools"].([]any); ok {
		for _, entry := range tools {
			tool, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			function, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			schema, ok := function["parameters"].(map[string]any)
			if !ok {
				continue
			}
			if rewritten, tree, ok := p.rewriteObjectSchema(schema); ok {
				function["parameters"] = rewritten
				name, _ := function["name"].(string)
				plan.addTool(name, tree)
				changed = true
			}
		}
	}

	if !changed {
		return nil, nil
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, nil
	}
	return encoded, plan
}

func (p *Proxy) prepareAnthropic(document map[string]any) ([]byte, *requestPlan) {
	tools, ok := document["tools"].([]any)
	if !ok {
		return nil, nil
	}
	plan := newRequestPlan()
	changed := false
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		schema, ok := tool["input_schema"].(map[string]any)
		if !ok {
			continue
		}
		if rewritten, tree, ok := p.rewriteObjectSchema(schema); ok {
			tool["input_schema"] = rewritten
			name, _ := tool["name"].(string)
			plan.addTool(name, tree)
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, nil
	}
	return encoded, plan
}

// rewriteSchema compiles a raw schema and returns the rewritten form when the
// gate says the savings pay for the legend.
func (p *Proxy) rewriteSchema(schema map[string]any) (map[string]any, *compiler.RehydrateNode, bool) {
	node := schemaFromMap(schema)
	plan, ok := compiler.Compile(node)
	if !ok {
		return nil, nil, false
	}
	if !p.allow(plan, node) {
		return nil, nil, false
	}
	rewritten, err := nodeToMap(plan.Rewritten)
	if err != nil {
		return nil, nil, false
	}
	return rewritten, plan.Rehydrate, true
}

// rewriteObjectSchema compiles a single repeated record (function arguments).
func (p *Proxy) rewriteObjectSchema(schema map[string]any) (map[string]any, *compiler.RehydrateNode, bool) {
	node := schemaFromMap(schema)
	plan, ok := compiler.CompileObject(node)
	if !ok {
		return nil, nil, false
	}
	if !p.allow(plan, node) {
		return nil, nil, false
	}
	rewritten, err := nodeToMap(plan.Rewritten)
	if err != nil {
		return nil, nil, false
	}
	return rewritten, plan.Rehydrate, true
}

func (p *Proxy) allow(plan compiler.CompiledPlan, node *compiler.SchemaNode) bool {
	legendTokens := p.cfg.LegendTokens
	if legendTokens <= 0 {
		legendTokens = int64(len(plan.Legend)+3) / 4
	}
	decision := p.gate.Decide(gate.Input{Schema: node, ExpectedInstances: 4, LegendTokens: legendTokens})
	return decision.Activate
}

func (p *Proxy) transformJSON(path string, payload []byte, plan *requestPlan) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, err
	}

	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		if choices, ok := document["choices"].([]any); ok {
			for _, entry := range choices {
				choice, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				message, ok := choice["message"].(map[string]any)
				if !ok {
					continue
				}
				if content, ok := message["content"].(string); ok && content != "" && plan.content != nil {
					message["content"] = rehydrateString(plan.content, content)
				}
				if toolCalls, ok := message["tool_calls"].([]any); ok {
					for _, call := range toolCalls {
						callMap, ok := call.(map[string]any)
						if !ok {
							continue
						}
						function, ok := callMap["function"].(map[string]any)
						if !ok {
							continue
						}
						name, _ := function["name"].(string)
						tree := plan.tool(name)
						if tree == nil {
							continue
						}
						if args, ok := function["arguments"].(string); ok {
							function["arguments"] = rehydrateString(tree, args)
						}
					}
				}
			}
		}
	case strings.HasSuffix(path, "/v1/messages"):
		if content, ok := document["content"].([]any); ok {
			for _, entry := range content {
				block, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				switch block["type"] {
				case "tool_use":
					name, _ := block["name"].(string)
					tree := plan.tool(name)
					if tree == nil {
						continue
					}
					if input, ok := block["input"]; ok {
						raw, err := json.Marshal(input)
						if err != nil {
							continue
						}
						if transformed, err := rehydrator.Transform(tree, raw); err == nil {
							var decoded any
							if json.Unmarshal(transformed, &decoded) == nil {
								block["input"] = decoded
							}
						}
					}
				case "text":
					if plan.content == nil {
						continue
					}
					if text, ok := block["text"].(string); ok && text != "" {
						block["text"] = rehydrateString(plan.content, text)
					}
				}
			}
		}
	}

	return json.Marshal(document)
}

func rehydrateString(plan *compiler.RehydrateNode, value string) string {
	transformed, err := rehydrator.Transform(plan, []byte(value))
	if err != nil {
		return value
	}
	return string(transformed)
}

// pipeSSE forwards server-sent events, rehydrating JSON deltas as they arrive.
func (p *Proxy) pipeSSE(w io.Writer, body io.Reader, plan *requestPlan, ctx context.Context, flush http.Flusher) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	state := newStreamState(plan)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			if _, err := w.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				return err
			}
			if len(bytes.TrimSpace(line)) == 0 && flush != nil {
				flush.Flush()
			}
			continue
		}

		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || payload[0] != '{' {
			if _, err := w.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				return err
			}
			continue
		}

		rewritten, err := state.rewriteEvent(payload)
		if err != nil {
			rewritten = payload
		}
		out := append([]byte("data: "), rewritten...)
		out = append(out, '\n')
		if _, err := w.Write(out); err != nil {
			return err
		}
		if flush != nil {
			flush.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	state.finish()
	return nil
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func requestStreams(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

func singleJoin(base, path string) string {
	base = strings.TrimSuffix(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// copyHeaders copies end-to-end headers only. Credentials are forwarded but
// never logged.
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if hopByHop[lower] || lower == "content-length" || lower == "accept-encoding" {
			continue
		}
		if lower == "x-forwarded-for" || lower == "x-forwarded-host" || lower == "x-real-ip" {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyBody(w io.Writer, body io.Reader, ctx context.Context) error {
	buf := make([]byte, 32<<10)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

var errBodyTooLarge = errors.New("body too large")

// maxResponseBytes bounds a non-streaming upstream body: four times the request
// cap, with a 16 MiB floor.
func maxResponseBytes(maxBody int64) int64 {
	limit := maxBody * 4
	if limit < 16<<20 {
		limit = 16 << 20
	}
	return limit
}

func readBounded(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

func (p *Proxy) record(path string, plan *requestPlan, started time.Time) {
	if p.cfg.Shadow == nil {
		return
	}
	mode := "passthrough"
	activated := plan != nil && !plan.empty()
	if activated {
		mode = "activated"
	}
	_ = p.cfg.Shadow.Record(ShadowRecord{
		TS:           started,
		Path:         path,
		Mode:         mode,
		Activated:    activated,
		LatencyAdded: time.Since(started),
	})
}

func schemaFromMap(schema map[string]any) *compiler.SchemaNode {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var node compiler.SchemaNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}
	return &node
}

func nodeToMap(node *compiler.SchemaNode) (map[string]any, error) {
	raw, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
