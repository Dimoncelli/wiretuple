// Package compiler turns object schemas into positional tuple schemas and
// builds the reversible plan used to rehydrate responses.
//
// A repeated object
//
//	{"items": [{"id": "a1", "total": 12.5}, {"id": "a2", "total": 3}]}
//
// is bijective with
//
//	{"items": [["a1", 12.5], ["a2", 3]]}
//
// given the legend "Tuple: [id, total]".
package compiler

import (
	"bytes"
	"encoding/json"
	"sort"
)

// SchemaNode is a lossless JSON Schema node. Unknown keys are kept in Extra and
// re-emitted by MarshalJSON so schemas round-trip without losing information.
type SchemaNode struct {
	Type            string                 `json:"type,omitempty"`
	Properties      map[string]*SchemaNode `json:"properties,omitempty"`
	Items           *SchemaNode            `json:"items,omitempty"`
	PrefixItems     []*SchemaNode          `json:"prefixItems,omitempty"`
	AdditionalItems *bool                  `json:"additionalItems,omitempty"`
	// ItemsFalse emits {"items": false}: the Draft 2020-12 way to close a
	// prefixItems tuple (Structured Outputs rejects additionalItems).
	ItemsFalse  bool                       `json:"-"`
	Required    []string                   `json:"required,omitempty"`
	Description string                     `json:"description,omitempty"`
	Enum        []json.RawMessage          `json:"enum,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// knownFields lists the struct fields in their deterministic marshal order.
var knownFields = []string{
	"type", "properties", "items", "prefixItems", "additionalItems",
	"required", "description", "enum",
}

// MarshalJSON re-emits known fields in a fixed order followed by unknown keys
// sorted lexicographically, so output is deterministic.
func (n *SchemaNode) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(n.Extra)+len(knownFields))

	set := func(key string, value any) error {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = b
		return nil
	}

	if n.Type != "" {
		if err := set("type", n.Type); err != nil {
			return nil, err
		}
	}
	if n.Properties != nil {
		if err := set("properties", n.Properties); err != nil {
			return nil, err
		}
	}
	if n.ItemsFalse {
		fields["items"] = json.RawMessage("false")
	} else if n.Items != nil {
		if err := set("items", n.Items); err != nil {
			return nil, err
		}
	}
	if n.PrefixItems != nil {
		if err := set("prefixItems", n.PrefixItems); err != nil {
			return nil, err
		}
	}
	if n.AdditionalItems != nil {
		if err := set("additionalItems", *n.AdditionalItems); err != nil {
			return nil, err
		}
	}
	if n.Required != nil {
		if err := set("required", n.Required); err != nil {
			return nil, err
		}
	}
	if n.Description != "" {
		if err := set("description", n.Description); err != nil {
			return nil, err
		}
	}
	if n.Enum != nil {
		if err := set("enum", n.Enum); err != nil {
			return nil, err
		}
	}
	for key, raw := range n.Extra {
		if _, exists := fields[key]; exists {
			continue
		}
		fields[key] = raw
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(encodedKey)
		buf.WriteByte(':')
		buf.Write(fields[key])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON captures known fields and stores every other key in Extra
// verbatim (raw bytes), so a round-trip preserves unknown keys.
func (n *SchemaNode) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}

	*n = SchemaNode{}
	for key, value := range raw {
		switch key {
		case "type":
			if err := json.Unmarshal(value, &n.Type); err != nil {
				return err
			}
		case "properties":
			if err := json.Unmarshal(value, &n.Properties); err != nil {
				return err
			}
		case "items":
			if string(value) == "false" {
				n.ItemsFalse = true
				continue
			}
			if err := json.Unmarshal(value, &n.Items); err != nil {
				return err
			}
		case "prefixItems":
			if err := json.Unmarshal(value, &n.PrefixItems); err != nil {
				return err
			}
		case "additionalItems":
			var flag bool
			if err := json.Unmarshal(value, &flag); err != nil {
				return err
			}
			n.AdditionalItems = &flag
		case "required":
			if err := json.Unmarshal(value, &n.Required); err != nil {
				return err
			}
		case "description":
			if err := json.Unmarshal(value, &n.Description); err != nil {
				return err
			}
		case "enum":
			if err := json.Unmarshal(value, &n.Enum); err != nil {
				return err
			}
		default:
			if n.Extra == nil {
				n.Extra = make(map[string]json.RawMessage)
			}
			n.Extra[key] = value
		}
	}
	return nil
}

// RehydrateKind marks what the response tree expects at one position.
type RehydrateKind int

const (
	// KindValue is a leaf: string, number, boolean or null.
	KindValue RehydrateKind = iota
	// KindObject is a JSON object.
	KindObject
	// KindArray is an ordinary array (no tuple transformation).
	KindArray
	// KindTupleArray is an array whose elements are positional tuples.
	KindTupleArray
	// KindTuple is a single positional tuple at this exact position, used when
	// one repeated record (for example function-call arguments) is converted.
	KindTuple
)

// RehydrateNode mirrors the response tree and marks where tuples live.
type RehydrateNode struct {
	Kind   RehydrateKind
	Keys   []string                  // non-nil only for KindTupleArray
	Fields map[string]*RehydrateNode // KindObject
	Item   *RehydrateNode            // KindArray / KindTupleArray element
}

// CompiledPlan is the full reversible description of one schema rewrite.
type CompiledPlan struct {
	Legend    string
	Rewritten *SchemaNode
	Rehydrate *RehydrateNode
	TupleKeys []string
}

// objectOnlyExtra are object-level keywords that must not leak onto an array
// schema after the node is converted into tuple form.
var objectOnlyExtra = map[string]bool{
	"additionalProperties": true,
	"minProperties":        true,
	"maxProperties":        true,
	"patternProperties":    true,
	"propertyNames":        true,
	"dependencies":         true,
}

// Compile rewrites schema into the positional tuple form and returns the plan
// needed to rehydrate responses. ok is false when nothing is tuple-eligible.
//
// The node passed in is treated as the position whose response shape must be
// rehydrated: an array of objects becomes a tuple array, an object with only
// leaf properties becomes a tuple array, and an object containing an array of
// objects is treated as a wrapper whose nested arrays are rewritten in place.
func Compile(schema *SchemaNode) (CompiledPlan, bool) {
	if schema == nil {
		return CompiledPlan{}, false
	}

	// The node passed in is a position whose own shape must be preserved; only
	// nested object arrays are rewritten. Use CompileObject when the position
	// itself is a repeated record (for example tool-call arguments).
	rewritten, tree, legend, keys, ok := compileNode(schema, false)
	if !ok {
		return CompiledPlan{}, false
	}
	return CompiledPlan{
		Legend:    legend,
		Rewritten: rewritten,
		Rehydrate: tree,
		TupleKeys: keys,
	}, true
}

// CompileObject rewrites an object schema into a tuple array. It is used for
// positions that are a single repeated record, such as function-call arguments,
// where keys repeat across calls and positional values save tokens.
func CompileObject(node *SchemaNode) (CompiledPlan, bool) {
	if !isObject(node) || len(node.Properties) == 0 {
		return CompiledPlan{}, false
	}
	rewritten, tree, legend, keys, ok := compileNode(node, true)
	if !ok {
		return CompiledPlan{}, false
	}
	if tree != nil && tree.Kind == KindTupleArray {
		// The position holds one tuple, not an array of tuples.
		tree.Kind = KindTuple
	}
	return CompiledPlan{
		Legend:    legend,
		Rewritten: rewritten,
		Rehydrate: tree,
		TupleKeys: keys,
	}, true
}

// compileNode returns the rewritten schema and its rehydration tree.
// convertObject reports whether this node itself may become a tuple array.
func compileNode(node *SchemaNode, convertObject bool) (
	rewritten *SchemaNode,
	tree *RehydrateNode,
	legend string,
	keys []string,
	ok bool,
) {
	switch {
	case isObject(node):
		if convertObject && !hasNestedTupleCandidate(node) {
			return convertToTuple(node)
		}
		return rewriteObject(node)
	case isArray(node):
		return rewriteArray(node)
	default:
		return copyLeaf(node), &RehydrateNode{Kind: KindValue}, "", nil, false
	}
}

// convertToTuple rewrites an object schema into an array of positional values.
func convertToTuple(node *SchemaNode) (
	*SchemaNode,
	*RehydrateNode,
	string,
	[]string,
	bool,
) {
	order := orderedKeys(node)
	if len(order) == 0 {
		return copyLeaf(node), &RehydrateNode{Kind: KindValue}, "", nil, false
	}

	prefix := make([]*SchemaNode, 0, len(order))
	fields := make(map[string]*RehydrateNode, len(order))
	anyConverted := false
	for _, key := range order {
		child := node.Properties[key]
		if isObject(child) && len(child.Properties) >= 2 {
			// Nested records become nested single tuples (prefixItems + items:false).
			nestedRewritten, nestedTree, _, _, nestedOK := convertToTuple(child)
			if nestedOK {
				nestedTree.Kind = KindTuple
				prefix = append(prefix, nestedRewritten)
				fields[key] = nestedTree
				continue
			}
		}
		childRewritten, childTree, _, _, _ := compileNode(child, false)
		prefix = append(prefix, childRewritten)
		fields[key] = childTree
	}

	rewritten := &SchemaNode{
		Type:        "array",
		PrefixItems: prefix,
		ItemsFalse:  true,
		Description: legendFor(order, node.Description),
	}
	for key, raw := range node.Extra {
		if objectOnlyExtra[key] {
			continue
		}
		if rewritten.Extra == nil {
			rewritten.Extra = make(map[string]json.RawMessage)
		}
		rewritten.Extra[key] = raw
	}

	// A tuple of a single object with primitive values is still a win when the
	// schema repeats inside an array; report ok so the caller can decide.
	anyConverted = true

	tree := &RehydrateNode{Kind: KindTupleArray, Keys: order, Item: &RehydrateNode{Kind: KindObject, Fields: fields}}
	return rewritten, tree, legendFor(order, ""), order, anyConverted
}

// rewriteObject keeps the object shape and rewrites nested arrays in place.
func rewriteObject(node *SchemaNode) (
	*SchemaNode,
	*RehydrateNode,
	string,
	[]string,
	bool,
) {
	rewritten := &SchemaNode{
		Type:        node.Type,
		Description: node.Description,
		Required:    append([]string(nil), node.Required...),
		Enum:        node.Enum,
		Extra:       cloneExtra(node.Extra),
	}
	tree := &RehydrateNode{Kind: KindObject, Fields: make(map[string]*RehydrateNode, len(node.Properties))}
	if node.Properties != nil {
		rewritten.Properties = make(map[string]*SchemaNode, len(node.Properties))
	}

	ok := false
	legend := ""
	var keys []string
	for key, child := range node.Properties {
		childRewritten, childTree, childLegend, childKeys, childOK := compileNode(child, false)
		rewritten.Properties[key] = childRewritten
		tree.Fields[key] = childTree
		if childOK && !ok {
			ok = true
			legend = childLegend
			keys = childKeys
		}
	}
	if node.Items != nil {
		itemsRewritten, itemsTree, itemsLegend, itemsKeys, itemsOK := compileNode(node.Items, false)
		rewritten.Items = itemsRewritten
		if itemsOK && !ok {
			ok = true
			legend = itemsLegend
			keys = itemsKeys
		}
		if itemsOK {
			tree.Fields["#items"] = itemsTree
		}
	}
	return rewritten, tree, legend, keys, ok
}

// rewriteArray converts an array of objects into a tuple array, or keeps an
// ordinary array while rewriting what is inside it.
func rewriteArray(node *SchemaNode) (
	*SchemaNode,
	*RehydrateNode,
	string,
	[]string,
	bool,
) {
	if node.Items != nil && isTupleEligibleObject(node.Items) {
		rewrittenItems, itemTree, legend, keys, ok := convertToTuple(node.Items)
		if ok {
			rewritten := &SchemaNode{
				Type:        "array",
				PrefixItems: rewrittenItems.PrefixItems,
				ItemsFalse:  true,
				Description: legendFor(keys, node.Description),
				Extra:       cloneExtra(node.Extra),
			}
			tree := &RehydrateNode{Kind: KindTupleArray, Keys: keys, Item: itemTree.Item}
			return rewritten, tree, legend, keys, true
		}
		_ = rewrittenItems
		_ = itemTree
	}

	rewritten := &SchemaNode{
		Type:        node.Type,
		Description: node.Description,
		Enum:        node.Enum,
		Extra:       cloneExtra(node.Extra),
	}
	tree := &RehydrateNode{Kind: KindArray, Item: &RehydrateNode{Kind: KindValue}}
	ok := false
	legend := ""
	var keys []string
	if node.Items != nil {
		itemsRewritten, itemsTree, itemsLegend, itemsKeys, itemsOK := compileNode(node.Items, false)
		rewritten.Items = itemsRewritten
		tree.Item = itemsTree
		if itemsOK {
			ok = true
			legend = itemsLegend
			keys = itemsKeys
		}
	}
	return rewritten, tree, legend, keys, ok
}

// hasNestedTupleCandidate reports whether the object contains an array of
// objects, which means the object itself is a wrapper and must not be
// collapsed into a tuple.
func hasNestedTupleCandidate(node *SchemaNode) bool {
	for _, child := range node.Properties {
		if isArray(child) && child.Items != nil && isTupleEligibleObject(child.Items) {
			return true
		}
		if isObject(child) && hasNestedTupleCandidate(child) {
			return true
		}
	}
	return false
}

func isTupleEligibleObject(node *SchemaNode) bool {
	return isObject(node) && len(node.Properties) > 0
}

func isObject(node *SchemaNode) bool {
	return node != nil && (node.Type == "object" || (node.Type == "" && node.Properties != nil))
}

func isArray(node *SchemaNode) bool {
	return node != nil && (node.Type == "array" || (node.Type == "" && (node.Items != nil || node.PrefixItems != nil)))
}

// orderedKeys applies the deterministic ordering rule: required keys first in
// schema order, then the remaining keys sorted lexicographically.
func orderedKeys(node *SchemaNode) []string {
	if node == nil || len(node.Properties) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(node.Properties))
	order := make([]string, 0, len(node.Properties))
	for _, key := range node.Required {
		if _, exists := node.Properties[key]; !exists || seen[key] {
			continue
		}
		seen[key] = true
		order = append(order, key)
	}
	rest := make([]string, 0, len(node.Properties))
	for key := range node.Properties {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// OrderedKeys exposes the deterministic key order for gates and benchmarks.
func OrderedKeys(node *SchemaNode) []string { return orderedKeys(node) }

func legendFor(keys []string, existing string) string {
	legend := "Tuple: ["
	for i, key := range keys {
		if i > 0 {
			legend += ", "
		}
		legend += key
	}
	legend += "]"
	if existing != "" {
		return legend + " " + existing
	}
	return legend
}

// copyLeaf deep-copies a schema node without transforming it.
func copyLeaf(node *SchemaNode) *SchemaNode {
	if node == nil {
		return nil
	}
	out := &SchemaNode{
		Type:        node.Type,
		Description: node.Description,
		ItemsFalse:  node.ItemsFalse,
		Required:    append([]string(nil), node.Required...),
		Enum:        node.Enum,
		Extra:       cloneExtra(node.Extra),
	}
	if node.Properties != nil {
		out.Properties = make(map[string]*SchemaNode, len(node.Properties))
		for key, child := range node.Properties {
			out.Properties[key] = copyLeaf(child)
		}
	}
	if node.Items != nil {
		out.Items = copyLeaf(node.Items)
	}
	if node.PrefixItems != nil {
		out.PrefixItems = make([]*SchemaNode, len(node.PrefixItems))
		for i, child := range node.PrefixItems {
			out.PrefixItems[i] = copyLeaf(child)
		}
	}
	if node.AdditionalItems != nil {
		flag := *node.AdditionalItems
		out.AdditionalItems = &flag
	}
	return out
}

func cloneExtra(extra map[string]json.RawMessage) map[string]json.RawMessage {
	if len(extra) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(extra))
	for key, raw := range extra {
		out[key] = raw
	}
	return out
}
