package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type captured struct {
	mu   sync.Mutex
	body []byte
	auth string
}

func (c *captured) set(body []byte, auth string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append([]byte(nil), body...)
	c.auth = auth
}

func (c *captured) get() ([]byte, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body, c.auth
}

func newProxy(t *testing.T, upstream *httptest.Server, mutate func(*Config)) *Proxy {
	t.Helper()
	cfg := Config{Upstream: upstream.URL, Activate: true, BreakEvenPct: 5}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return p
}

func heavySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":            map[string]any{"type": "string"},
						"sku":           map[string]any{"type": "string"},
						"name":          map[string]any{"type": "string"},
						"quantity":      map[string]any{"type": "integer"},
						"unit_price":    map[string]any{"type": "number"},
						"currency_code": map[string]any{"type": "string"},
					},
					"required": []any{"id", "sku", "name", "quantity", "unit_price", "currency_code"},
				},
			},
		},
	}
}

func TestOpenAINonStreamRewritesAndRehydrates(t *testing.T) {
	store := &captured{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		store.set(body, r.Header.Get("Authorization"))
		content := `{"items":[["a1","sku-1","Widget",2,12.5,"EUR"],["a2","sku-2","Gadget",1,3,"USD"]]}`
		response := map[string]any{
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	requestBody := map[string]any{
		"model":    "test-model",
		"messages": []any{map[string]any{"role": "user", "content": "invoice"}},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "invoice", "schema": heavySchema()},
		},
	}
	encoded, _ := json.Marshal(requestBody)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer test-token")
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	sent, auth := store.get()
	if auth != "Bearer test-token" {
		t.Fatalf("authorization not forwarded: %q", auth)
	}
	if !bytes.Contains(sent, []byte("prefixItems")) {
		t.Fatalf("upstream did not see tuple schema: %s", sent)
	}
	if !bytes.Contains(sent, []byte("Tuple: [")) {
		t.Fatalf("legend missing upstream: %s", sent)
	}

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	content := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	var invoice struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(content), &invoice); err != nil {
		t.Fatalf("client content is not JSON: %s", content)
	}
	if len(invoice.Items) != 2 || invoice.Items[0]["sku"] != "sku-1" {
		t.Fatalf("rehydration failed: %s", content)
	}
}

func TestOpenAIFunctionCallingArguments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		response := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":       "call_1",
						"type":     "function",
						"function": map[string]any{"name": "plan_trip", "arguments": `["2026-05-01","Lisbon",4]`},
					}},
				},
			}},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body := map[string]any{
		"model":    "test-model",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "plan_trip",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"destination_city": map[string]any{"type": "string"},
						"trip_duration":    map[string]any{"type": "integer"},
						"departure_date":   map[string]any{"type": "string"},
					},
				},
			},
		}},
	}
	encoded, _ := json.Marshal(body)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	proxy.ServeHTTP(recorder, request)

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	call := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"].(string)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		t.Fatalf("arguments not JSON: %s", arguments)
	}
	if parsed["departure_date"] != "2026-05-01" {
		t.Fatalf("arguments not rehydrated: %s", arguments)
	}
}

func TestAnthropicToolInputRehydrated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		response := map[string]any{
			"type": "message",
			"content": []any{map[string]any{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "plan_trip",
				"input": []any{"2026-05-01", "Lisbon", 4},
			}},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body := map[string]any{
		"model":      "test-model",
		"max_tokens": 512,
		"tools": []any{map[string]any{
			"name": "plan_trip",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"destination_city": map[string]any{"type": "string"},
					"trip_duration":    map[string]any{"type": "integer"},
					"departure_date":   map[string]any{"type": "string"},
				},
			},
		}},
	}
	encoded, _ := json.Marshal(body)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(encoded))
	proxy.ServeHTTP(recorder, request)

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	block := decoded["content"].([]any)[0].(map[string]any)
	input, ok := block["input"].(map[string]any)
	if !ok {
		t.Fatalf("tool input was not rehydrated: %#v", block["input"])
	}
	if input["destination_city"] != "Lisbon" {
		t.Fatalf("unexpected input: %#v", input)
	}
}

func TestBelowThresholdIsByteIdentical(t *testing.T) {
	var sawBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"{\"items\":[[1]]}"}}]}`))
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, func(cfg *Config) { cfg.Activate = false })
	body := []byte(`{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}}}}}}}`)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	proxy.ServeHTTP(recorder, request)

	if !bytes.Equal(sawBody, body) {
		t.Fatalf("request was rewritten while inactive:\n%s", sawBody)
	}
}

func TestSSEStreamRehydratesAcrossDeltas(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		// The JSON document is split inside a tuple element and inside a string.
		fragments := []string{`{"items":[["a1","sku`, `-1","Wid`, `get",2,12.5,"EUR"],["a2","sku-2","Gadget",1,3,"USD"]]}`}
		for _, fragment := range fragments {
			encoded, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": fragment}}},
			})
			_, _ = w.Write(append([]byte("data: "), encoded...))
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	requestBody := map[string]any{
		"model":    "test-model",
		"stream":   true,
		"messages": []any{},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": heavySchema()},
		},
	}
	encoded, _ := json.Marshal(requestBody)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	proxy.ServeHTTP(recorder, request)

	var assembled strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(recorder.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}
		choices, _ := event["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			assembled.WriteString(content)
		}
	}

	var invoice struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(assembled.String()), &invoice); err != nil {
		t.Fatalf("assembled stream is not JSON: %s", assembled.String())
	}
	if len(invoice.Items) != 2 || invoice.Items[1]["name"] != "Gadget" {
		t.Fatalf("stream not rehydrated: %s", assembled.String())
	}
}

func TestClientCancelDoesNotLeakGoroutines(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{\\\"items\\\":[[\"a1\",\"sku-1\",\"Widget\",2,12.5,\"EUR\"]]}\"}}]}\n\n"))
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	proxy := newProxy(t, upstream, nil)
	before := runtime.NumGoroutine()

	server := httptest.NewServer(proxy)
	defer server.Close()

	body := map[string]any{
		"model":    "test-model",
		"stream":   true,
		"messages": []any{},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": heavySchema()},
		},
	}
	encoded, _ := json.Marshal(body)

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(encoded))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	buffer := make([]byte, 64)
	_, _ = response.Body.Read(buffer)
	response.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestBodyLimitReturns413(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, func(cfg *Config) { cfg.MaxBodyBytes = 64 })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", 200)))
	proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
}

func TestNewRejectsInvalidUpstream(t *testing.T) {
	if _, err := New(Config{Upstream: "ftp://example.com"}); err == nil {
		t.Fatal("expected scheme rejection")
	}
	if _, err := New(Config{Upstream: ""}); err == nil {
		t.Fatal("expected empty upstream rejection")
	}
}
