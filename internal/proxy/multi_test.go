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

func toolSchema(keys ...string) map[string]any {
	properties := map[string]any{}
	for _, key := range keys {
		properties[key] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": properties}
}

func openAIToolsRequest(tools ...map[string]any) []byte {
	entries := make([]any, 0, len(tools))
	for _, tool := range tools {
		entries = append(entries, map[string]any{"type": "function", "function": tool})
	}
	body, _ := json.Marshal(map[string]any{"model": "test-model", "messages": []any{}, "tools": entries})
	return body
}

func hotelsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"request": map[string]any{"type": "object", "properties": map[string]any{
				"city": map[string]any{"type": "string"},
				"area": map[string]any{"type": "string"},
			}},
			"nights": map[string]any{"type": "integer"},
		},
	}
}

func TestMultiToolOpenAINonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		response := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{"id": "c1", "type": "function", "function": map[string]any{
							"name": "plan_trip", "arguments": `["2026-05-01","Lisbon",4]`,
						}},
						map[string]any{"id": "c2", "type": "function", "function": map[string]any{
							"name": "search_hotels", "arguments": `[2,["Baixa","Lisbon"]]`,
						}},
					},
				},
			}},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body := openAIToolsRequest(
		map[string]any{"name": "plan_trip", "parameters": toolSchema("departure_date", "destination_city", "trip_duration")},
		map[string]any{"name": "search_hotels", "parameters": hotelsSchema()},
	)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	proxy.ServeHTTP(recorder, request)

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	calls := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)

	first := calls[0].(map[string]any)["function"].(map[string]any)
	var firstArgs map[string]any
	if err := json.Unmarshal([]byte(first["arguments"].(string)), &firstArgs); err != nil {
		t.Fatalf("first arguments: %v", err)
	}
	if firstArgs["departure_date"] != "2026-05-01" || firstArgs["trip_duration"] != float64(4) {
		t.Fatalf("tool #1 keys wrong: %#v", firstArgs)
	}

	second := calls[1].(map[string]any)["function"].(map[string]any)
	var secondArgs map[string]any
	if err := json.Unmarshal([]byte(second["arguments"].(string)), &secondArgs); err != nil {
		t.Fatalf("second arguments: %v", err)
	}
	requestPayload, ok := secondArgs["request"].(map[string]any)
	if !ok || requestPayload["city"] != "Lisbon" {
		t.Fatalf("tool #2 keys wrong: %#v", secondArgs)
	}
	if secondArgs["nights"] != float64(2) {
		t.Fatalf("tool #2 nights wrong: %#v", secondArgs)
	}
}

func TestMultiToolOpenAIStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		events := []map[string]any{
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "plan_trip"}}},
			}}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `["2026-05-01"`}}},
			}}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `,"Lisbon",4]`}}},
			}}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 1, "function": map[string]any{"name": "search_hotels"}}},
			}}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 1, "function": map[string]any{"arguments": `[2,["Baixa","Lisbon"]]`}}},
			}}}},
		}
		for _, event := range events {
			encoded, _ := json.Marshal(event)
			_, _ = w.Write(append([]byte("data: "), encoded...))
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body := openAIToolsRequest(
		map[string]any{"name": "plan_trip", "parameters": toolSchema("departure_date", "destination_city", "trip_duration")},
		map[string]any{"name": "search_hotels", "parameters": hotelsSchema()},
	)
	requestBody := map[string]any{}
	_ = json.Unmarshal(body, &requestBody)
	requestBody["stream"] = true
	encoded, _ := json.Marshal(requestBody)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	proxy.ServeHTTP(recorder, request)

	assembled := map[string]string{}
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
		calls, _ := delta["tool_calls"].([]any)
		for _, entry := range calls {
			call, _ := entry.(map[string]any)
			index := int(call["index"].(float64))
			function, _ := call["function"].(map[string]any)
			if arguments, ok := function["arguments"].(string); ok {
				assembled[stringKey(index)] += arguments
			}
		}
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(assembled["0"]), &first); err != nil {
		t.Fatalf("streamed tool #1 not JSON: %q (%v)", assembled["0"], err)
	}
	if first["destination_city"] != "Lisbon" {
		t.Fatalf("tool #1 keys wrong: %#v", first)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(assembled["1"]), &second); err != nil {
		t.Fatalf("streamed tool #2 not JSON: %q (%v)", assembled["1"], err)
	}
	if second["nights"] != float64(2) {
		t.Fatalf("tool #2 keys wrong: %#v", second)
	}
}

func TestMultiToolAnthropicNonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		response := map[string]any{
			"type": "message",
			"content": []any{
				map[string]any{"type": "tool_use", "name": "plan_trip", "id": "t1", "input": []any{"2026-05-01", "Lisbon", 4}},
				map[string]any{"type": "tool_use", "name": "search_hotels", "id": "t2", "input": []any{2, []any{"Baixa", "Lisbon"}}},
			},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "test-model", "max_tokens": 256,
		"tools": []any{
			map[string]any{"name": "plan_trip", "input_schema": toolSchema("departure_date", "destination_city", "trip_duration")},
			map[string]any{"name": "search_hotels", "input_schema": hotelsSchema()},
		},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	proxy.ServeHTTP(recorder, request)

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	blocks := decoded["content"].([]any)
	first := blocks[0].(map[string]any)["input"].(map[string]any)
	if first["departure_date"] != "2026-05-01" {
		t.Fatalf("anthropic tool #1 wrong: %#v", first)
	}
	second := blocks[1].(map[string]any)["input"].(map[string]any)
	requestPayload, ok := second["request"].(map[string]any)
	if !ok || requestPayload["area"] != "Baixa" {
		t.Fatalf("anthropic tool #2 wrong: %#v", second)
	}
}

func TestMultiToolAnthropicStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		events := []map[string]any{
			{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": "plan_trip"}},
			{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `["2026-05-01","Lisbon",4]`}},
			{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "name": "search_hotels"}},
			{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `[2,["Baixa","Lisbon"]]`}},
		}
		for _, event := range events {
			encoded, _ := json.Marshal(event)
			_, _ = w.Write(append([]byte("data: "), encoded...))
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "test-model", "max_tokens": 256, "stream": true,
		"tools": []any{
			map[string]any{"name": "plan_trip", "input_schema": toolSchema("departure_date", "destination_city", "trip_duration")},
			map[string]any{"name": "search_hotels", "input_schema": hotelsSchema()},
		},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	proxy.ServeHTTP(recorder, request)

	assembled := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(recorder.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}
		delta, ok := event["delta"].(map[string]any)
		if !ok {
			continue
		}
		partial, ok := delta["partial_json"].(string)
		if !ok {
			continue
		}
		index := int(event["index"].(float64))
		assembled[stringKey(index)] += partial
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(assembled["0"]), &first); err != nil {
		t.Fatalf("anthropic stream tool #1: %q (%v)", assembled["0"], err)
	}
	if first["departure_date"] != "2026-05-01" {
		t.Fatalf("anthropic stream tool #1 keys wrong: %#v", first)
	}
	if !strings.Contains(assembled["1"], `"request"`) {
		t.Fatalf("anthropic stream tool #2 keys wrong: %q", assembled["1"])
	}
}

func TestToolArgumentsWrongArityStayValidJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		response := map[string]any{"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{"tool_calls": []any{map[string]any{
				"type": "function", "function": map[string]any{"name": "plan_trip", "arguments": `["only-one"]`},
			}}},
		}}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	body := openAIToolsRequest(map[string]any{"name": "plan_trip", "parameters": toolSchema("departure_date", "destination_city", "trip_duration")})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	proxy.ServeHTTP(recorder, request)

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	call := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"].(string)
	if !json.Valid([]byte(arguments)) {
		t.Fatalf("fallback arguments are not valid JSON: %q", arguments)
	}
}

func TestConcurrentCancelledStreamsDoNotLeak(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{\\\"items\\\":[]}\"}}]}\n\n"))
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	proxy := newProxy(t, upstream, nil)
	server := httptest.NewServer(proxy)
	defer server.Close()

	before := runtime.NumGoroutine()
	body := map[string]any{
		"model": "test-model", "stream": true, "messages": []any{},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"schema": heavySchema()}},
	}
	encoded, _ := json.Marshal(body)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(encoded))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return
			}
			buffer := make([]byte, 32)
			_, _ = response.Body.Read(buffer)
			response.Body.Close()
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+3 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestProxyHeadersAreStripped(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"{}"}}]}`))
	}))
	defer upstream.Close()

	proxy := newProxy(t, upstream, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m","messages":[]}`)))
	request.Header.Set("Authorization", "Bearer secret-token")
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Forwarded-Host", "spoofed.example")
	request.Header.Set("X-Real-IP", "203.0.113.9")
	proxy.ServeHTTP(recorder, request)

	if got.Get("Authorization") != "Bearer secret-token" {
		t.Fatal("Authorization must be forwarded")
	}
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-IP"} {
		if got.Get(header) != "" {
			t.Fatalf("%s must be stripped, got %q", header, got.Get(header))
		}
	}
}

func TestMaxResponseBytes(t *testing.T) {
	if got := maxResponseBytes(1 << 20); got != 16<<20 {
		t.Fatalf("floor not applied: %d", got)
	}
	if got := maxResponseBytes(8 << 20); got != 32<<20 {
		t.Fatalf("cap not scaled: %d", got)
	}
}
