package blocks

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Bounds on how much of a payload this package will ever walk. A card is a
// small, human-authored document; a hostile or pathological one - a million
// entry array, a few thousand levels of nesting - degrades to a truncated
// render rather than a hang or a stack overflow.
const (
	// maxDepth caps how many containers deep (ColumnSet -> Column -> Container
	// -> ...) the Adaptive Card renderer will recurse.
	maxDepth = 12
	// maxNodes caps how many elements, across the whole payload, get rendered.
	maxNodes = 500
	// maxTextRunes caps a single text field before it is parsed or escaped.
	maxTextRunes = 4000
)

// budget is shared by one call to Render and threaded through every
// recursive renderer, so the total amount of markup produced is bounded
// regardless of how the payload is shaped.
type budget struct {
	left int
}

func newBudget() *budget { return &budget{left: maxNodes} }

// take reports whether there is budget left for one more element, and spends
// it if so.
func (b *budget) take() bool {
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// decodeAny unmarshals data into a generic JSON value (map[string]any,
// []any, string, float64, bool or nil), returning false for empty or
// malformed input.
func decodeAny(data json.RawMessage) (any, bool) {
	if len(data) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, false
	}
	return v, true
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asSlice(v any) ([]any, bool) {
	s, ok := v.([]any)
	return s, ok
}

func getStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	mm, _ := m[key].(map[string]any)
	return mm
}

func getSlice(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	s, _ := m[key].([]any)
	return s
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// getNumber reads key as a JSON number, accepting a numeric string too since
// some providers (and Slack's own "ts") send timestamps as strings.
func getNumber(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	switch t := m[key].(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}
