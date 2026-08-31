package components_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/core/server/ui/components"
)

func TestNewJSONViewBuildsATree(t *testing.T) {
	v := components.NewJSONView("Payload", map[string]any{
		"subject": "hi",
		"to":      []any{"a@example.com", "b@example.com"},
		"nested":  map[string]any{"n": 1, "ok": true, "nothing": nil},
	})

	if v.Root.Kind != "object" || v.Root.Length != 3 {
		t.Fatalf("root = %+v", v.Root)
	}
	// Keys are sorted, so the same document always renders the same way.
	var keys []string
	for _, c := range v.Root.Children {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, ",") != "nested,subject,to" {
		t.Errorf("keys = %v, want them sorted", keys)
	}

	byKey := map[string]components.JSONNode{}
	for _, c := range v.Root.Children {
		byKey[c.Key] = c
	}
	if byKey["to"].Kind != "array" || byKey["to"].Length != 2 {
		t.Errorf("to = %+v", byKey["to"])
	}
	if byKey["to"].Children[0].Key != "[0]" {
		t.Errorf("array children must be indexed: %+v", byKey["to"].Children[0])
	}
	if byKey["subject"].Kind != "string" || byKey["subject"].Value != "hi" {
		t.Errorf("subject = %+v", byKey["subject"])
	}

	nested := byKey["nested"]
	kinds := map[string]string{}
	for _, c := range nested.Children {
		kinds[c.Key] = c.Kind
	}
	if kinds["n"] != "number" || kinds["ok"] != "bool" || kinds["nothing"] != "null" {
		t.Errorf("nested kinds = %v", kinds)
	}
	if !strings.Contains(v.Raw, `"subject": "hi"`) {
		t.Errorf("Raw must be the pretty document behind the copy button:\n%s", v.Raw)
	}
}

func TestNewJSONViewHandlesUnencodableValues(t *testing.T) {
	v := components.NewJSONView("Payload", make(chan int))
	if !strings.Contains(v.Root.Value, "unencodable") {
		t.Errorf("root = %+v; a bad payload must not take the page down", v.Root)
	}
}

func TestNewHexView(t *testing.T) {
	data := []byte{0x00, 0x41, 0x42, 0xff}
	v := components.NewHexView("Body", data, 0)
	if len(v.Rows) != 1 {
		t.Fatalf("rows = %d", len(v.Rows))
	}
	row := v.Rows[0]
	if row.Offset != "00000000" {
		t.Errorf("offset = %q", row.Offset)
	}
	if len(row.Hex) != 16 {
		t.Errorf("a short row must still be padded to 16 columns, got %d", len(row.Hex))
	}
	if strings.Join(row.Hex[:4], " ") != "00 41 42 ff" {
		t.Errorf("hex = %v", row.Hex[:4])
	}
	if row.ASCII != ".AB." {
		t.Errorf("ascii = %q, want unprintables as dots", row.ASCII)
	}
	if v.Truncated {
		t.Error("a 4 byte body is not truncated")
	}

	big := components.NewHexView("Body", make([]byte, 100), 32)
	if !big.Truncated || big.Shown != 32 || big.Size != 100 {
		t.Errorf("truncation = %+v", struct {
			T    bool
			S, Z int
		}{big.Truncated, big.Shown, big.Size})
	}
	if len(big.Rows) != 2 {
		t.Errorf("rows = %d, want 2 for 32 shown bytes", len(big.Rows))
	}
}

func TestIsTextBody(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, true},
		{"ascii", []byte("hello world"), true},
		{"json", []byte(`{"a":1}`), true},
		{"utf8", []byte("héllo → wörld"), true},
		{"newlines and tabs", []byte("a\r\n\tb"), true},
		{"nul byte", []byte{'a', 0, 'b'}, false},
		{"binary", []byte{0xff, 0xfe, 0x01, 0x02, 0x03, 0x04}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := components.IsTextBody(tc.in); got != tc.want {
				t.Errorf("IsTextBody(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBytesHuman(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"}, {512, "512 B"}, {2048, "2.0 KB"}, {5 << 20, "5.0 MB"}, {3 << 30, "3.0 GB"},
	}
	for _, tc := range tests {
		if got := components.BytesHuman(tc.in); got != tc.want {
			t.Errorf("BytesHuman(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		n    int
		in   string
		want string
	}{
		{10, "short", "short"},
		{5, "abcdefgh", "abcd…"},
		{5, "héllo wörld", "héll…"},
		{0, "abc", ""},
	}
	for _, tc := range tests {
		if got := components.Truncate(tc.n, tc.in); got != tc.want {
			t.Errorf("Truncate(%d, %q) = %q, want %q", tc.n, tc.in, got, tc.want)
		}
	}
}

func TestDict(t *testing.T) {
	m, err := components.Dict("a", 1, "b", "two")
	if err != nil {
		t.Fatalf("dict: %v", err)
	}
	if m["a"] != 1 || m["b"] != "two" {
		t.Errorf("dict = %v", m)
	}
	if _, err := components.Dict("a"); err == nil {
		t.Error("an odd argument count must be an error")
	}
	if _, err := components.Dict(1, 2); err == nil {
		t.Error("a non-string key must be an error")
	}
}

func TestTemplateSetParsesAndBinds(t *testing.T) {
	tpl, err := components.Template()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, name := range []string{
		"badge", "copy-button", "kv-table", "table",
		"json-inspector", "hex-viewer", "raw-viewer",
		"master-detail", "stream",
		"how-to-test", "empty-state", "snippet", "provider-card",
		"event-list", "event-detail", "generic-event-view",
	} {
		if tpl.Lookup(name) == nil {
			t.Errorf("component %q is missing; plugin tabs are meant to compose from it", name)
		}
	}

	// render must be bound, otherwise every layout would fail at execution.
	var sb strings.Builder
	err = tpl.ExecuteTemplate(&sb, "master-detail", map[string]any{
		"Title": "T",
		"List":  "",
		"Detail": func() any {
			v, _ := components.Dict()
			return v
		}(),
	})
	if err != nil {
		t.Fatalf("execute master-detail: %v", err)
	}
	if !strings.Contains(sb.String(), `id="detail"`) {
		t.Errorf("master-detail = %s", sb.String())
	}
}
