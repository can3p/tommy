package blocks

import (
	"encoding/json"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"testing"
	"time"
)

// allowedOutputTags is every element this package's non-test code ever
// writes. Payload text always reaches the page through html.EscapeString,
// which turns a literal "<" into "&lt;" - so any *literal* "<...>" left in
// the rendered output is a real tag the browser will parse, and the only way
// dangerousMarkup should ever see one outside this set is a bug that skipped
// escaping somewhere.
var allowedOutputTags = map[string]bool{
	"div": true, "span": true, "img": true, "a": true, "hr": true,
	"pre": true, "code": true, "strong": true, "em": true, "s": true,
	"p": true, "br": true,
}

var (
	openTagRe     = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)([^<>]*)>`)
	closeTagRe    = regexp.MustCompile(`</([a-zA-Z][a-zA-Z0-9]*)\s*>`)
	onHandlerRe   = regexp.MustCompile(`(?i)\bon[a-z]+\s*=`)
	hrefRe        = regexp.MustCompile(`(?i)\bhref\s*=\s*"([^"]*)"`)
	srcRe         = regexp.MustCompile(`(?i)\bsrc\s*=\s*"([^"]*)"`)
	quotedValueRe = regexp.MustCompile(`="[^"]*"`)
)

// attrNameArea returns attrs with every ="..." value blanked out, so a
// scan for an attribute *name* (like an on-handler) does not trip over that
// same text merely quoted as the value of a legitimate attribute such as
// alt="...onerror=...". Payload text that lands in alt/href/src is exactly
// this shape once escaped: the dangerous-looking substring is inert quoted
// text, not a second attribute.
func attrNameArea(attrs string) string {
	return quotedValueRe.ReplaceAllString(attrs, `=""`)
}

// dangerousMarkup parses the real (literal, unescaped) tags out of html and
// fails the test if any of them is outside allowedOutputTags (which is how a
// <script>, <style>, <iframe> or an attacker-introduced element like <svg
// onload=...> gets caught), if any tag carries an on*="..." event-handler
// attribute, or if any href/src attribute resolves to a javascript:,
// vbscript: or data: URL. It deliberately does not do a naive substring scan
// for strings like "javascript:" or "onerror=", because those are exactly
// the payloads this suite plants in ordinary text fields - once escaped they
// are inert page text, not markup, and flagging them there would make the
// suite fail on the safe case it exists to verify.
func dangerousMarkup(t *testing.T, html template.HTML) {
	t.Helper()
	s := string(html)

	for _, m := range closeTagRe.FindAllStringSubmatch(s, -1) {
		if !allowedOutputTags[strings.ToLower(m[1])] {
			t.Fatalf("unexpected closing tag </%s> in output:\n%s", m[1], s)
		}
	}

	for _, m := range openTagRe.FindAllStringSubmatch(s, -1) {
		name, attrs := strings.ToLower(m[1]), m[2]
		if !allowedOutputTags[name] {
			t.Fatalf("unexpected tag <%s> in output:\n%s", name, s)
		}
		if onHandlerRe.MatchString(attrNameArea(attrs)) {
			t.Fatalf("event-handler attribute on <%s> in output:\n%s", name, s)
		}
		for _, am := range hrefRe.FindAllStringSubmatch(attrs, -1) {
			checkURLAttr(t, am[1], s)
		}
		for _, am := range srcRe.FindAllStringSubmatch(attrs, -1) {
			checkURLAttr(t, am[1], s)
		}
	}
}

func checkURLAttr(t *testing.T, url, full string) {
	t.Helper()
	lower := strings.ToLower(strings.TrimSpace(url))
	for _, bad := range []string{"javascript:", "vbscript:", "data:"} {
		if strings.HasPrefix(lower, bad) {
			t.Fatalf("dangerous URL scheme %q reached an href/src attribute: %q\n%s", bad, url, full)
		}
	}
}

// xssStrings are the payloads dropped into every text/label/name field this
// package touches.
var xssStrings = []string{
	`<script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`<img src=x onerror=alert(document.cookie)>`,
	`</div><svg onload=alert(1)>`,
	`javascript:alert(1)`,
	`'"--></style></script><script>alert(1)</script>`,
	`<a href="javascript:alert(1)">click</a>`,
	`&lt;script&gt;alert(1)&lt;/script&gt;`,
	"\x00<script>alert(1)</script>",
	`{{7*7}}`,
	`${alert(1)}`,
}

// hostileURLs are the URL-slot payloads: things that must never become a
// live href/src, or must at least never execute.
var hostileURLs = []string{
	`javascript:alert(1)`,
	`JaVaScRiPt:alert(1)`,
	"java\nscript:alert(1)",
	"java\tscript:alert(1)",
	`vbscript:msgbox(1)`,
	`data:text/html,<script>alert(1)</script>`,
	`data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==`,
	`data:image/svg+xml,<svg onload=alert(1)>`,
	`file:///etc/passwd`,
	`//evil.example.com/x`,
	``,
	`   `,
	`not a url`,
	`http://`,
	`https:`,
}

func runXSSMatrix(t *testing.T, name string, build func(payload string) json.RawMessage, format string) {
	t.Run(name, func(t *testing.T) {
		for i, xss := range xssStrings {
			t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
				data := build(xss)
				html, ok := Render(format, data)
				if !ok {
					return // fine: renderer chose not to handle this shape at all
				}
				dangerousMarkup(t, html)
			})
		}
	})
}

func runURLMatrix(t *testing.T, name string, build func(url string) json.RawMessage, format string) {
	t.Run(name, func(t *testing.T) {
		for i, u := range hostileURLs {
			t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
				data := build(u)
				html, ok := Render(format, data)
				if !ok {
					return
				}
				s := string(html)
				dangerousMarkup(t, html)
				if u != "" && strings.Contains(s, `href="`+u+`"`) {
					t.Fatalf("hostile URL %q was passed through verbatim into href: %s", u, s)
				}
				if u != "" && strings.Contains(s, `src="`+u+`"`) {
					t.Fatalf("hostile URL %q was passed through verbatim into src: %s", u, s)
				}
			})
		}
	})
}

func rawf(format string, args ...any) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(format, args...))
}

func jstr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- Slack blocks -----------------------------------------------------

func TestHostile_SlackBlocks_TextFields(t *testing.T) {
	runXSSMatrix(t, "section_text", func(p string) json.RawMessage {
		return rawf(`[{"type":"section","text":{"type":"mrkdwn","text":%s}}]`, jstr(p))
	}, formatSlackBlocks)
	runXSSMatrix(t, "header_text", func(p string) json.RawMessage {
		return rawf(`[{"type":"header","text":{"type":"plain_text","text":%s}}]`, jstr(p))
	}, formatSlackBlocks)
	runXSSMatrix(t, "field_text", func(p string) json.RawMessage {
		return rawf(`[{"type":"section","fields":[{"type":"mrkdwn","text":%s}]}]`, jstr(p))
	}, formatSlackBlocks)
	runXSSMatrix(t, "context_text", func(p string) json.RawMessage {
		return rawf(`[{"type":"context","elements":[{"type":"mrkdwn","text":%s}]}]`, jstr(p))
	}, formatSlackBlocks)
	runXSSMatrix(t, "button_label", func(p string) json.RawMessage {
		return rawf(`[{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":%s}}]}]`, jstr(p))
	}, formatSlackBlocks)
	runXSSMatrix(t, "alt_text", func(p string) json.RawMessage {
		return rawf(`[{"type":"image","image_url":"https://example.com/x.png","alt_text":%s}]`, jstr(p))
	}, formatSlackBlocks)
	runXSSMatrix(t, "mention_label", func(p string) json.RawMessage {
		return rawf(`[{"type":"section","text":{"type":"mrkdwn","text":"<@U1|%s>"}}]`, strings.ReplaceAll(p, `"`, ``))
	}, formatSlackBlocks)
}

func TestHostile_SlackBlocks_URLFields(t *testing.T) {
	runURLMatrix(t, "accessory_image_url", func(u string) json.RawMessage {
		return rawf(`[{"type":"section","text":{"type":"plain_text","text":"hi"},"accessory":{"type":"image","image_url":%s,"alt_text":"a"}}]`, jstr(u))
	}, formatSlackBlocks)
	runURLMatrix(t, "top_level_image_url", func(u string) json.RawMessage {
		return rawf(`[{"type":"image","image_url":%s,"alt_text":"a"}]`, jstr(u))
	}, formatSlackBlocks)
	runURLMatrix(t, "context_image_url", func(u string) json.RawMessage {
		return rawf(`[{"type":"context","elements":[{"type":"image","image_url":%s,"alt_text":"a"}]}]`, jstr(u))
	}, formatSlackBlocks)
	runURLMatrix(t, "mrkdwn_link", func(u string) json.RawMessage {
		return rawf(`[{"type":"section","text":{"type":"mrkdwn","text":%s}}]`, jstr("<"+u+"|click>"))
	}, formatSlackBlocks)
}

// --- Slack attachments --------------------------------------------------

func TestHostile_SlackAttachments_TextFields(t *testing.T) {
	fields := []string{"pretext", "title", "text", "author_name", "footer"}
	for _, f := range fields {
		field := f
		runXSSMatrix(t, field, func(p string) json.RawMessage {
			return rawf(`[{%s:%s}]`, jstr(field), jstr(p))
		}, formatSlackAttachments)
	}
	runXSSMatrix(t, "field_value", func(p string) json.RawMessage {
		return rawf(`[{"fields":[{"title":"k","value":%s}]}]`, jstr(p))
	}, formatSlackAttachments)
	runXSSMatrix(t, "color", func(p string) json.RawMessage {
		return rawf(`[{"color":%s,"text":"hi"}]`, jstr(p))
	}, formatSlackAttachments)
}

func TestHostile_SlackAttachments_URLFields(t *testing.T) {
	urlFields := []string{"author_link", "title_link", "image_url", "thumb_url", "author_icon", "footer_icon"}
	for _, f := range urlFields {
		field := f
		runURLMatrix(t, field, func(u string) json.RawMessage {
			return rawf(`[{"text":"hi",%s:%s}]`, jstr(field), jstr(u))
		}, formatSlackAttachments)
	}
}

// --- MessageCard ---------------------------------------------------------

func TestHostile_MessageCard_TextFields(t *testing.T) {
	fields := []string{"title", "text", "themeColor"}
	for _, f := range fields {
		field := f
		runXSSMatrix(t, field, func(p string) json.RawMessage {
			return rawf(`{%s:%s}`, jstr(field), jstr(p))
		}, formatTeamsMessageCard)
	}
	runXSSMatrix(t, "section_activityTitle", func(p string) json.RawMessage {
		return rawf(`{"sections":[{"activityTitle":%s}]}`, jstr(p))
	}, formatTeamsMessageCard)
	runXSSMatrix(t, "fact_value", func(p string) json.RawMessage {
		return rawf(`{"sections":[{"facts":[{"name":"k","value":%s}]}]}`, jstr(p))
	}, formatTeamsMessageCard)
	runXSSMatrix(t, "potentialAction_name", func(p string) json.RawMessage {
		return rawf(`{"text":"x","potentialAction":[{"name":%s}]}`, jstr(p))
	}, formatTeamsMessageCard)
}

func TestHostile_MessageCard_URLFields(t *testing.T) {
	runURLMatrix(t, "activityImage", func(u string) json.RawMessage {
		return rawf(`{"sections":[{"activityImage":%s,"activityTitle":"x"}]}`, jstr(u))
	}, formatTeamsMessageCard)
}

// --- Adaptive Card ---------------------------------------------------------

func TestHostile_AdaptiveCard_TextFields(t *testing.T) {
	runXSSMatrix(t, "textblock", func(p string) json.RawMessage {
		return rawf(`{"body":[{"type":"TextBlock","text":%s}]}`, jstr(p))
	}, formatTeamsAdaptiveCard)
	runXSSMatrix(t, "factset_value", func(p string) json.RawMessage {
		return rawf(`{"body":[{"type":"FactSet","facts":[{"title":"k","value":%s}]}]}`, jstr(p))
	}, formatTeamsAdaptiveCard)
	runXSSMatrix(t, "action_title", func(p string) json.RawMessage {
		return rawf(`{"actions":[{"type":"Action.OpenUrl","title":%s,"url":"https://example.com"}]}`, jstr(p))
	}, formatTeamsAdaptiveCard)
	runXSSMatrix(t, "image_altText", func(p string) json.RawMessage {
		return rawf(`{"body":[{"type":"Image","url":"https://example.com/x.png","altText":%s}]}`, jstr(p))
	}, formatTeamsAdaptiveCard)
	runXSSMatrix(t, "link_markdown", func(p string) json.RawMessage {
		safe := strings.ReplaceAll(strings.ReplaceAll(p, "]", ""), ")", "")
		return rawf(`{"body":[{"type":"TextBlock","text":%s}]}`, jstr("[click]("+safe+")"))
	}, formatTeamsAdaptiveCard)
}

func TestHostile_AdaptiveCard_URLFields(t *testing.T) {
	runURLMatrix(t, "image_url", func(u string) json.RawMessage {
		return rawf(`{"body":[{"type":"Image","url":%s,"altText":"a"}]}`, jstr(u))
	}, formatTeamsAdaptiveCard)
	runURLMatrix(t, "action_url", func(u string) json.RawMessage {
		return rawf(`{"actions":[{"type":"Action.OpenUrl","title":"go","url":%s}]}`, jstr(u))
	}, formatTeamsAdaptiveCard)
	runURLMatrix(t, "markdown_link", func(u string) json.RawMessage {
		return rawf(`{"body":[{"type":"TextBlock","text":%s}]}`, jstr("[click]("+u+")"))
	}, formatTeamsAdaptiveCard)
}

// --- Structural hostility: depth, size, malformed, wrong types, null ------

func TestHostile_DeepNesting_AdaptiveCard(t *testing.T) {
	// A Container nested a few thousand levels deep must not blow the stack.
	depth := 5000
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"type":"Container","items":[`)
	}
	b.WriteString(`{"type":"TextBlock","text":"bottom"}`)
	for i := 0; i < depth; i++ {
		b.WriteString(`]}`)
	}
	data := rawf(`{"body":[%s]}`, b.String())

	done := make(chan struct{})
	var html template.HTML
	var ok bool
	go func() {
		defer close(done)
		html, ok = Render(formatTeamsAdaptiveCard, data)
	}()
	select {
	case <-done:
		if ok {
			dangerousMarkup(t, html)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Render did not return within 10s on deeply nested Adaptive Card")
	}
}

func TestHostile_DeepNesting_ColumnSets(t *testing.T) {
	depth := 2000
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"type":"ColumnSet","columns":[{"type":"Column","items":[`)
	}
	b.WriteString(`{"type":"TextBlock","text":"bottom"}`)
	for i := 0; i < depth; i++ {
		b.WriteString(`]}]}`)
	}
	data := rawf(`{"body":[%s]}`, b.String())
	html, ok := Render(formatTeamsAdaptiveCard, data)
	if ok {
		dangerousMarkup(t, html)
	}
}

func TestHostile_HugeArray_SlackBlocks(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 50000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"divider"}`)
	}
	b.WriteString("]")
	html, ok := Render(formatSlackBlocks, json.RawMessage(b.String()))
	if !ok {
		t.Fatal("expected huge-but-valid blocks array to render something")
	}
	if strings.Count(string(html), "<hr") > maxNodes+5 {
		t.Fatalf("huge array was not bounded: got %d <hr> elements", strings.Count(string(html), "<hr"))
	}
}

func TestHostile_HugeArray_Fields(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[{"title":"k","value":"v","fields":[`)
	for i := 0; i < 50000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"k","value":"v"}`)
	}
	b.WriteString(`]}]`)
	html, ok := Render(formatSlackAttachments, json.RawMessage(b.String()))
	if !ok {
		t.Fatal("expected valid attachments with a huge fields array to render something")
	}
	dangerousMarkup(t, html)
}

func TestHostile_MalformedAndTruncatedJSON(t *testing.T) {
	inputs := []string{
		``,
		`{`,
		`[`,
		`{"body":`,
		`{"body":[{"type":"TextBlock","text":"unterminat`,
		"\xff\xfe\x00garbage",
		`{"body": [1, 2, 3]}`,          // items are scalars, not objects
		`{"body": "not an array"}`,     // wrong type
		`{"body": null}`,               // null field
		`{"body": [null, null, null]}`, // null items
		`{"body": [{"type": null}]}`,   // null type
		`{"body": [{"type": 42}]}`,     // wrong type for type
		`{"actions": "nope"}`,          // wrong type
		`null`,
		`true`,
		`false`,
		`0`,
		`""`,
	}
	for _, format := range []string{formatSlackBlocks, formatSlackAttachments, formatTeamsMessageCard, formatTeamsAdaptiveCard} {
		for _, in := range inputs {
			html, ok := Render(format, json.RawMessage(in))
			if ok {
				dangerousMarkup(t, html)
			}
		}
	}
}

func TestHostile_WrongTypesEverywhere(t *testing.T) {
	// Every field that should be a string, object or array is instead a
	// number, bool, array-where-object-expected or object-where-array-expected.
	payloads := []string{
		`[{"type":"section","text":123}]`,
		`[{"type":"section","text":[1,2,3]}]`,
		`[{"type":"section","text":true}]`,
		`[{"type":"section","fields":"nope"}]`,
		`[{"type":"section","fields":{"a":1}}]`,
		`[{"type":"section","accessory":[1,2]}]`,
		`[{"type":"actions","elements":{"not":"array"}}]`,
		`[{"type":"context","elements":42}]`,
		`[42, "str", true, null, {"type":"divider"}]`,
	}
	for _, p := range payloads {
		html, ok := Render(formatSlackBlocks, json.RawMessage(p))
		if ok {
			dangerousMarkup(t, html)
		}
	}

	acPayloads := []string{
		`{"body": [{"type":"FactSet","facts":"nope"}]}`,
		`{"body": [{"type":"FactSet","facts":[1,2,3]}]}`,
		`{"body": [{"type":"ColumnSet","columns":"nope"}]}`,
		`{"body": [{"type":"ColumnSet","columns":[{"items":"nope"}]}]}`,
		`{"body": [{"type":"Container","items":42}]}`,
		`{"body": [{"type":"Image","url":42}]}`,
		`{"actions": [{"title":123,"url":true}]}`,
	}
	for _, p := range acPayloads {
		html, ok := Render(formatTeamsAdaptiveCard, json.RawMessage(p))
		if ok {
			dangerousMarkup(t, html)
		}
	}
}

func TestHostile_NullTopLevel(t *testing.T) {
	for _, format := range []string{formatSlackBlocks, formatSlackAttachments, formatTeamsMessageCard, formatTeamsAdaptiveCard} {
		html, ok := Render(format, json.RawMessage(`null`))
		if ok || html != "" {
			t.Errorf("Render(%q, null) = %q, %v; want false", format, html, ok)
		}
	}
}

func TestHostile_NeverPanics(t *testing.T) {
	inputs := []string{
		`[{"type":"section","text":{"type":"mrkdwn","text":"` + strings.Repeat("*", 100000) + `"}}]`,
		`[{"type":"section","text":{"type":"mrkdwn","text":"` + strings.Repeat("<@U1|x>", 20000) + `"}}]`,
		`[{"type":"section","text":{"type":"mrkdwn","text":"` + strings.Repeat("`", 50000) + `"}}]`,
		`{"body":[{"type":"TextBlock","text":"` + strings.Repeat("*", 100000) + `"}]}`,
		`{"body":[{"type":"TextBlock","text":"` + strings.Repeat("[a](b)", 20000) + `"}]}`,
	}
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("input %d panicked: %v", i, r)
				}
			}()
			format := formatSlackBlocks
			if strings.HasPrefix(in, "{") {
				format = formatTeamsAdaptiveCard
			}
			Render(format, json.RawMessage(in))
		}()
	}
}
