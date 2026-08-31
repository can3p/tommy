// Package components is the shared template library every plugin tab composes
// from: page chrome, master-detail and stream layouts, a JSON inspector, a hex
// viewer for binary bodies, tables, badges, copy buttons - and the generic
// event view any plugin gets for free.
//
// The point is that a new protocol plugin is useful on day one with zero UI
// code, and a bespoke view is an upgrade rather than a prerequisite.
package components

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//go:embed *.html
var templatesFS embed.FS

// FS exposes the component templates, so a plugin can parse them into its own
// template set.
func FS() fs.FS { return templatesFS }

// Badge is a small coloured label: a provider name, a status, an encoding.
type Badge struct {
	Label string
	// Tone is one of "", "info", "ok", "warn", "error", "muted".
	Tone  string
	Title string
}

// KV is one row of a key/value table.
type KV struct {
	Key   string
	Value string
	// HTML, when set, is used instead of Value.
	HTML template.HTML
}

// KVTable is a key/value table with an optional caption.
type KVTable struct {
	Caption string
	Rows    []KV
}

// Cell is one table cell. HTML wins over Text when both are set.
type Cell struct {
	Text  string
	HTML  template.HTML
	Class string
}

// Row is one table row. Href, when set, makes the row a link target for htmx.
type Row struct {
	Cells    []Cell
	Href     string
	Target   string
	Class    string
	Selected bool
}

// Table is a plain data table.
type Table struct {
	Caption string
	Columns []string
	Rows    []Row
	Empty   string
}

// MasterDetail is the list-plus-detail layout: a scrolling list on the left, a
// detail pane on the right.
type MasterDetail struct {
	Title   string
	Toolbar template.HTML
	List    template.HTML
	ListID  string
	Detail  template.HTML
	// DetailID lets htmx swap only the detail pane.
	DetailID string
	Empty    template.HTML
}

// Stream is the chronological layout: a single column of items, newest first,
// used by conversation-shaped plugins.
type Stream struct {
	Title   string
	Toolbar template.HTML
	ID      string
	Items   []template.HTML
	Empty   template.HTML
}

// JSONView is the collapsible JSON inspector.
type JSONView struct {
	Title string
	Root  JSONNode
	// Raw is the pretty-printed document behind the copy button.
	Raw string
}

// JSONNode is one node of the inspector tree.
type JSONNode struct {
	Key      string
	Kind     string // object, array, string, number, bool, null
	Value    string
	Children []JSONNode
	Length   int
	Depth    int
}

// NewJSONView renders any Go value into an inspector tree. Values are passed
// through encoding/json first, so what the inspector shows is exactly what the
// API would return.
func NewJSONView(title string, v any) JSONView {
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return JSONView{
			Title: title,
			Root:  JSONNode{Kind: "string", Value: fmt.Sprintf("<unencodable: %v>", err)},
		}
	}
	dec := json.NewDecoder(bytes.NewReader(pretty))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return JSONView{Title: title, Root: JSONNode{Kind: "string", Value: string(pretty)}, Raw: string(pretty)}
	}
	return JSONView{Title: title, Root: buildNode("", generic, 0), Raw: string(pretty)}
}

func buildNode(key string, v any, depth int) JSONNode {
	n := JSONNode{Key: key, Depth: depth}
	switch t := v.(type) {
	case nil:
		n.Kind, n.Value = "null", "null"
	case bool:
		n.Kind, n.Value = "bool", strconv.FormatBool(t)
	case string:
		n.Kind, n.Value = "string", t
	case json.Number:
		n.Kind, n.Value = "number", t.String()
	case float64:
		n.Kind, n.Value = "number", strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		n.Kind = "array"
		n.Length = len(t)
		for i, item := range t {
			n.Children = append(n.Children, buildNode("["+strconv.Itoa(i)+"]", item, depth+1))
		}
	case map[string]any:
		n.Kind = "object"
		n.Length = len(t)
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n.Children = append(n.Children, buildNode(k, t[k], depth+1))
		}
	default:
		n.Kind, n.Value = "string", fmt.Sprint(t)
	}
	return n
}

// DefaultHexLimit is how many bytes the hex viewer renders before truncating.
const DefaultHexLimit = 64 << 10

// HexRow is one 16-byte line of the hex viewer.
type HexRow struct {
	Offset string
	Hex    []string
	ASCII  string
}

// HexView is the hex viewer: what a binary Raw.Body falls back to.
type HexView struct {
	Title     string
	Rows      []HexRow
	Size      int
	Shown     int
	Truncated bool
}

// NewHexView builds a hex dump of b, truncated at limit bytes
// (DefaultHexLimit when limit <= 0).
func NewHexView(title string, b []byte, limit int) HexView {
	if limit <= 0 {
		limit = DefaultHexLimit
	}
	v := HexView{Title: title, Size: len(b)}
	data := b
	if len(data) > limit {
		data = data[:limit]
		v.Truncated = true
	}
	v.Shown = len(data)
	for off := 0; off < len(data); off += 16 {
		end := min(off+16, len(data))
		chunk := data[off:end]
		row := HexRow{Offset: fmt.Sprintf("%08x", off)}
		var ascii strings.Builder
		for i := 0; i < 16; i++ {
			if i < len(chunk) {
				row.Hex = append(row.Hex, fmt.Sprintf("%02x", chunk[i]))
				c := rune(chunk[i])
				if c < 0x20 || c > 0x7e {
					ascii.WriteByte('.')
				} else {
					ascii.WriteRune(c)
				}
			} else {
				row.Hex = append(row.Hex, "  ")
			}
		}
		row.ASCII = ascii.String()
		v.Rows = append(v.Rows, row)
	}
	return v
}

// IsTextBody guesses whether a body can be shown as text. Providers set
// event.Raw.Text explicitly; this is the fallback for the ones that do not.
func IsTextBody(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	sample := b
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	printable := 0
	for _, c := range string(sample) {
		if c == unicode.ReplacementChar {
			return false
		}
		if c == '\n' || c == '\r' || c == '\t' || unicode.IsPrint(c) {
			printable++
		}
	}
	return printable*10 >= len([]rune(string(sample)))*9
}

// FuncMap is the template helper set the components rely on. Plugins parsing
// the component templates must install it.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"jsonView": NewJSONView,
		"hexView":  func(title string, b []byte) HexView { return NewHexView(title, b, 0) },
		"isText":   IsTextBody,
		"pretty": func(v any) string {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				return fmt.Sprint(v)
			}
			return string(b)
		},
		"bytesHuman": BytesHuman,
		"truncate":   Truncate,
		"timeShort":  func(t time.Time) string { return t.Local().Format("15:04:05") },
		"timeFull":   func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05.000 MST") },
		"since":      func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
		"join":       strings.Join,
		"lower":      strings.ToLower,
		"asString":   func(b []byte) string { return string(b) },
		"dict":       Dict,
		"badge":      func(label, tone string) Badge { return Badge{Label: label, Tone: tone} },
		"kv":         func(k, v string) KV { return KV{Key: k, Value: v} },
		"add":        func(a, b int) int { return a + b },
		"hasPrefix":  strings.HasPrefix,
		"int64":      func(n int) int64 { return int64(n) },
		"rawMeta":    RawMeta,
		"summaryOf":  SummaryTable,
		// render is replaced by Bind; this placeholder only keeps parsing happy.
		"render": func(name string, data any) (template.HTML, error) {
			return "", fmt.Errorf("render %q: template set was not bound with components.Bind", name)
		},
	}
}

// Dict builds a map inside a template: {{template "x" (dict "a" 1 "b" 2)}}.
func Dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict needs an even number of arguments, got %d", len(values))
	}
	m := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict key %d is not a string", i)
		}
		m[key] = values[i+1]
	}
	return m, nil
}

// BytesHuman renders a byte count compactly.
func BytesHuman(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}

// Truncate shortens s to n runes, adding an ellipsis.
func Truncate(n int, s string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// Template returns a template set containing every component, with FuncMap
// installed and render bound. Plugins clone it and add their own templates.
func Template() (*template.Template, error) {
	t, err := template.New("components").Funcs(FuncMap()).ParseFS(templatesFS, "*.html")
	if err != nil {
		return nil, err
	}
	return Bind(t), nil
}

// Bind installs the render helper on a template set. It must be called on the
// final set - after cloning and after parsing a plugin's own templates -
// because render executes against the set it is bound to.
//
// render is what makes the layouts compositional: a layout template receives
// already-rendered fragments, so a plugin writes
//
//	{{template "master-detail" (dict "List" (render "my-list" .) ...)}}
func Bind(t *template.Template) *template.Template {
	t.Funcs(template.FuncMap{
		"render": func(name string, data any) (template.HTML, error) {
			var buf bytes.Buffer
			if err := t.ExecuteTemplate(&buf, name, data); err != nil {
				return "", fmt.Errorf("render %q: %w", name, err)
			}
			return template.HTML(buf.String()), nil
		},
	})
	return t
}
