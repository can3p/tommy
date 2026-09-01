package blocks

import (
	"os"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return b
}

func TestRender_UnknownFormat(t *testing.T) {
	html, ok := Render("something.unknown", []byte(`{"a":1}`))
	if ok || html != "" {
		t.Fatalf("expected unknown format to return false, got %q, %v", html, ok)
	}
}

func TestRender_MalformedJSON(t *testing.T) {
	cases := []string{
		formatSlackBlocks,
		formatSlackAttachments,
		formatTeamsMessageCard,
		formatTeamsAdaptiveCard,
	}
	malformed := []string{
		``,
		`{`,
		`[`,
		`not json at all`,
		`{"unterminated": "str`,
		`null`,
	}
	for _, format := range cases {
		for _, raw := range malformed {
			html, ok := Render(format, []byte(raw))
			if ok || html != "" {
				t.Errorf("Render(%q, %q) = %q, %v; want false", format, raw, html, ok)
			}
		}
	}
}

func TestRender_EmptyData(t *testing.T) {
	html, ok := Render(formatSlackBlocks, nil)
	if ok || html != "" {
		t.Fatalf("expected nil data to return false, got %q, %v", html, ok)
	}
}

func TestRender_WrongTopLevelShape(t *testing.T) {
	// Blocks/attachments want an array; MessageCard/AdaptiveCard want an object.
	cases := []struct {
		format string
		data   string
	}{
		{formatSlackBlocks, `{"type":"section"}`},           // object, not array
		{formatSlackBlocks, `"just a string"`},              // scalar
		{formatSlackBlocks, `42`},                           // scalar
		{formatSlackAttachments, `{"color":"#fff"}`},        // object, not array
		{formatTeamsMessageCard, `[1,2,3]`},                 // array, not object
		{formatTeamsMessageCard, `"MessageCard"`},           // scalar
		{formatTeamsAdaptiveCard, `[{"type":"TextBlock"}]`}, // array, not object
		{formatTeamsAdaptiveCard, `123`},                    // scalar
	}
	for _, c := range cases {
		html, ok := Render(c.format, []byte(c.data))
		if ok || html != "" {
			t.Errorf("Render(%q, %q) = %q, %v; want false", c.format, c.data, html, ok)
		}
	}
}

func TestRender_SlackBlocksGolden(t *testing.T) {
	data := readTestdata(t, "slack_blocks_basic.json")
	html, ok := Render(formatSlackBlocks, data)
	if !ok {
		t.Fatalf("Render() ok = false, want true")
	}
	s := string(html)
	wantAll(t, s,
		"Build failed",
		"<strong>nightly-build</strong>",
		"<code",
		"main",
		`href="https://ci.example.com/build/42"`,
		"build #42",
		`class="chat-mention"`,
		"@U0123ABCD",
		"Branch:",
		`<hr style=`,
		`src="https://example.com/avatar.png"`,
		"Posted by",
		"#builds",
		"Re-run",
		"Cancel",
		`src="https://example.com/graph.png"`,
		"Duration over time",
	)
	if strings.Contains(s, "<script") || strings.Contains(s, "javascript:") {
		t.Fatalf("output contains dangerous markup: %s", s)
	}
}

func TestRender_SlackAttachmentsGolden(t *testing.T) {
	data := readTestdata(t, "slack_attachments_basic.json")
	html, ok := Render(formatSlackAttachments, data)
	if !ok {
		t.Fatalf("Render() ok = false, want true")
	}
	s := string(html)
	wantAll(t, s,
		"Bobby Tables",
		`href="https://example.com/users/bobby"`,
		`src="https://example.com/icons/bobby.png"`,
		"<em>pretext</em>",
		`href="https://api.slack.com/"`,
		"Slack API Documentation",
		"<strong>text</strong>",
		"Priority",
		"High",
		"Environment",
		"production",
		`src="https://example.com/images/preview.png"`,
		`src="https://example.com/images/thumb.png"`,
		"Slack API",
		"2023-11-30",
	)
}

func TestRender_MessageCardGolden(t *testing.T) {
	data := readTestdata(t, "messagecard_basic.json")
	html, ok := Render(formatTeamsMessageCard, data)
	if !ok {
		t.Fatalf("Render() ok = false, want true")
	}
	s := string(html)
	wantAll(t, s,
		"<strong>production</strong>",
		"<code",
		"v1.4.2",
		"Jenkins",
		"Deploy pipeline",
		`src="https://example.com/jenkins.png"`,
		"Environment",
		"Duration",
		"2m31s",
		"All health checks passed.",
		"View build",
		"View logs",
	)
}

func TestRender_AdaptiveCardGolden(t *testing.T) {
	data := readTestdata(t, "adaptivecard_basic.json")
	html, ok := Render(formatTeamsAdaptiveCard, data)
	if !ok {
		t.Fatalf("Render() ok = false, want true")
	}
	s := string(html)
	wantAll(t, s,
		"Order",
		"<strong>#1234</strong>",
		"shipped",
		"<em>Alice Example</em>",
		"Order",
		"Carrier",
		"Left column",
		"Nested container text",
		`src="https://example.com/thumb.png"`,
		"Track shipment",
		"View order",
		"Mark received",
	)
}

func wantAll(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("output missing %q\n--- full output ---\n%s", n, haystack)
		}
	}
}
