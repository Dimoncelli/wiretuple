package compiler

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func mustParse(t *testing.T, raw string) *SchemaNode {
	t.Helper()
	var node SchemaNode
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return &node
}

func TestCompileObjectArrayKeepsRequiredOrder(t *testing.T) {
	schema := mustParse(t, `{
		"type": "object",
		"properties": {
			"items": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {"a": {"type": "integer"}, "b": {"type": "string"}},
					"required": ["b", "a"]
				}
			}
		}
	}`)

	plan, ok := Compile(schema)
	if !ok {
		t.Fatal("expected the schema to be tuple-eligible")
	}
	if plan.Legend != "Tuple: [b, a]" {
		t.Fatalf("legend = %q, want %q", plan.Legend, "Tuple: [b, a]")
	}
	if !reflect.DeepEqual(plan.TupleKeys, []string{"b", "a"}) {
		t.Fatalf("tuple keys = %v", plan.TupleKeys)
	}

	items := plan.Rewritten.Properties["items"]
	if items == nil || items.Type != "array" {
		t.Fatalf("items not rewritten as array: %+v", items)
	}
	if len(items.PrefixItems) != 2 {
		t.Fatalf("prefixItems length = %d, want 2", len(items.PrefixItems))
	}
	if items.PrefixItems[0].Type != "string" || items.PrefixItems[1].Type != "integer" {
		t.Fatalf("prefixItems order = %q, %q", items.PrefixItems[0].Type, items.PrefixItems[1].Type)
	}
	if !items.ItemsFalse {
		t.Fatal("tuple arrays must close with items:false (Draft 2020-12)")
	}
	if items.Type != "array" {
		t.Fatalf("items type = %q", items.Type)
	}
	if items.Description == "" || !bytes.HasPrefix([]byte(items.Description), []byte("Tuple:")) {
		t.Fatalf("description missing legend: %q", items.Description)
	}

	tree := plan.Rehydrate
	if tree.Kind != KindObject || tree.Fields["items"] == nil {
		t.Fatalf("unexpected rehydrate root: %+v", tree)
	}
	if tree.Fields["items"].Kind != KindTupleArray {
		t.Fatalf("items tree kind = %v", tree.Fields["items"].Kind)
	}
	if !reflect.DeepEqual(tree.Fields["items"].Keys, []string{"b", "a"}) {
		t.Fatalf("tree keys = %v", tree.Fields["items"].Keys)
	}
}

func TestCompileDirectObjectArray(t *testing.T) {
	schema := mustParse(t, `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {"id": {"type": "string"}, "total": {"type": "number"}}
		}
	}`)

	plan, ok := Compile(schema)
	if !ok {
		t.Fatal("direct object array must compile")
	}
	if plan.Legend != "Tuple: [id, total]" {
		t.Fatalf("legend = %q", plan.Legend)
	}
	if plan.Rewritten.Type != "array" || len(plan.Rewritten.PrefixItems) != 2 {
		t.Fatalf("rewritten root = %+v", plan.Rewritten)
	}
	if plan.Rehydrate.Kind != KindTupleArray {
		t.Fatalf("root kind = %v, want KindTupleArray", plan.Rehydrate.Kind)
	}
}

func TestCompileRejectsNonTupleSchemas(t *testing.T) {
	cases := map[string]string{
		"string":                `{"type": "string"}`,
		"empty properties":      `{"type": "object", "properties": {}}`,
		"array of strings":      `{"type": "array", "items": {"type": "string"}}`,
		"object without arrays": `{"type": "object", "properties": {"a": {"type": "string"}}}`,
	}
	for name, raw := range cases {
		if _, ok := Compile(mustParse(t, raw)); ok {
			t.Fatalf("%s: expected ok=false", name)
		}
	}
}

func TestCompileObjectForToolArguments(t *testing.T) {
	node := mustParse(t, `{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`)

	plan, ok := CompileObject(node)
	if !ok {
		t.Fatal("object arguments must compile")
	}
	if plan.Legend != "Tuple: [city, days]" {
		t.Fatalf("legend = %q", plan.Legend)
	}
	if plan.Rehydrate.Kind != KindTuple || len(plan.Rehydrate.Keys) != 2 {
		t.Fatalf("unexpected tree: %+v", plan.Rehydrate)
	}
	if plan.Rewritten.Type != "array" || len(plan.Rewritten.PrefixItems) != 2 {
		t.Fatalf("unexpected rewritten schema: %+v", plan.Rewritten)
	}

	if _, ok := CompileObject(mustParse(t, `{"type":"string"}`)); ok {
		t.Fatal("non-object must not compile as object")
	}
}

func TestUnknownKeysRoundTrip(t *testing.T) {
	raw := `{"type":"object","strict":true,"properties":{"a":{"type":"string","x-custom":"keep-me"}},"additionalProperties":false}`
	var node SchemaNode
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	first, err := json.Marshal(&node)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again SchemaNode
	if err := json.Unmarshal(first, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	second, err := json.Marshal(&again)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round-trip changed bytes:\n%s\n%s", first, second)
	}

	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["strict"] != true {
		t.Fatal("unknown key strict was lost")
	}
	props := decoded["properties"].(map[string]any)["a"].(map[string]any)
	if props["x-custom"] != "keep-me" {
		t.Fatal("nested unknown key was lost")
	}
}

func TestCompilePreservesWrapperExtrasAndDropsObjectOnlyOnTuple(t *testing.T) {
	schema := mustParse(t, `{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"records": {"type": "array", "items": {"type": "object", "properties": {"a": {"type": "string"}}}}
		}
	}`)
	plan, ok := Compile(schema)
	if !ok {
		t.Fatal("expected ok")
	}
	if plan.Rewritten.Type != "object" {
		t.Fatalf("wrapper must stay an object, got %q", plan.Rewritten.Type)
	}
	if _, exists := plan.Rewritten.Extra["additionalProperties"]; !exists {
		t.Fatal("wrapper lost additionalProperties")
	}
	if _, exists := plan.Rewritten.Properties["records"].Extra["additionalProperties"]; exists {
		t.Fatal("converted tuple kept object-only keyword")
	}
}

func TestCompileNestedTupleTree(t *testing.T) {
	schema := mustParse(t, `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"lines": {"type": "array", "items": {"type": "object", "properties": {"sku": {"type": "string"}, "qty": {"type": "integer"}}}}
			}
		}
	}`)

	plan, ok := Compile(schema)
	if !ok {
		t.Fatal("expected nested tuple plan")
	}
	if plan.Rehydrate.Kind != KindTupleArray {
		t.Fatalf("root kind = %v", plan.Rehydrate.Kind)
	}
	lines := plan.Rehydrate.Item.Fields["lines"]
	if lines == nil || lines.Kind != KindTupleArray {
		t.Fatalf("nested lines tree = %+v", lines)
	}
	if !reflect.DeepEqual(lines.Keys, []string{"qty", "sku"}) {
		t.Fatalf("nested keys = %v", lines.Keys)
	}
}

func TestOrderedKeysSortsNonRequired(t *testing.T) {
	node := mustParse(t, `{"type":"object","properties":{"z":{"type":"string"},"m":{"type":"string"},"a":{"type":"string"}},"required":["m"]}`)
	got := OrderedKeys(node)
	want := []string{"m", "a", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestTupleArraysCloseWithItemsFalse(t *testing.T) {
	plan, ok := Compile(mustParse(t, `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}}}}`))
	if !ok {
		t.Fatal("expected compilation")
	}
	raw, err := json.Marshal(plan.Rewritten)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"items":false`)) {
		t.Fatalf("tuple must emit items:false: %s", raw)
	}
	if bytes.Contains(raw, []byte("additionalItems")) {
		t.Fatalf("Draft 2020-12 must not emit additionalItems: %s", raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["items"] != false {
		t.Fatalf("items = %#v, want false", decoded["items"])
	}
}

func TestNestedObjectBecomesNestedTuple(t *testing.T) {
	plan, ok := Compile(mustParse(t, `{
		"type": "array",
		"items": {
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"address": {"type": "object", "properties": {"city": {"type": "string"}, "zip": {"type": "string"}}}
			}
		}
	}`))
	if !ok {
		t.Fatal("expected compilation")
	}

	address := plan.Rehydrate.Item.Fields["address"]
	if address == nil || address.Kind != KindTuple {
		t.Fatalf("address tree = %+v, want KindTuple", address)
	}
	if !reflect.DeepEqual(address.Keys, []string{"city", "zip"}) {
		t.Fatalf("address keys = %v", address.Keys)
	}

	addressIndex := -1
	for i, key := range plan.TupleKeys {
		if key == "address" {
			addressIndex = i
		}
	}
	if addressIndex < 0 {
		t.Fatalf("address missing from tuple keys: %v", plan.TupleKeys)
	}
	addressSchema := plan.Rewritten.PrefixItems[addressIndex]
	if addressSchema.Type != "array" || len(addressSchema.PrefixItems) != 2 || !addressSchema.ItemsFalse {
		t.Fatalf("nested schema not a tuple: %+v", addressSchema)
	}
	if addressSchema.Description != "Tuple: [city, zip]" {
		t.Fatalf("nested legend = %q", addressSchema.Description)
	}
}
