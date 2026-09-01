package blocks

import (
	"html"
	"net/url"
	"strings"
	"unicode/utf8"
)

// escapeHTML escapes s so it is safe to concatenate directly into an HTML
// text node or a double-quoted attribute value. Every string this package
// lifts out of a card payload - text, labels, alt text, ids - is untrusted
// input from the application under test and must go through this (or
// renderMrkdwn, which escapes internally) before it reaches the output.
func escapeHTML(s string) string {
	return html.EscapeString(s)
}

// allowedLinkSchemes is the scheme allowlist for an anchor href: a real link
// a person could click, or a mailto:.
var allowedLinkSchemes = map[string]bool{"http": true, "https": true, "mailto": true}

// allowedImageSchemes is the scheme allowlist for an image or icon src. Only
// fetchable http(s) URLs are allowed; data: URIs are rejected outright rather
// than sniffed, which is the simplest way to guarantee data:text/html and
// friends never reach an <img> or <a> attribute.
var allowedImageSchemes = map[string]bool{"http": true, "https": true}

// sanitizeURL validates raw against schemes and returns the normalized URL
// plus true, or "" and false if raw is empty, unparsable, has no host (for
// anything other than mailto:), or uses a scheme outside the allowlist -
// which is how javascript:, vbscript:, data: and any other executable or
// unexpected scheme are neutralized. Allowlisting the scheme, rather than
// denylisting known-bad ones, is deliberate: it fails closed on schemes
// nobody has thought of yet.
func sanitizeURL(raw string, schemes map[string]bool) (string, bool) {
	raw = stripControl(strings.TrimSpace(raw))
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" || !schemes[scheme] {
		return "", false
	}
	if scheme != "mailto" && u.Host == "" {
		return "", false
	}
	return u.String(), true
}

// stripControl removes control characters (including tab/newline/CR), which
// is enough to defeat scheme-obfuscation tricks like "java\nscript:" before
// the string ever reaches url.Parse.
func stripControl(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// safeLink renders an <a> tag for target/label if target validates as a link
// URL, and falls back to plain escaped text (label, else target) otherwise -
// so a bad URL degrades to inert text instead of vanishing or executing.
func safeLink(target, label string) string {
	disp := label
	if strings.TrimSpace(disp) == "" {
		disp = target
	}
	if u, ok := sanitizeURL(target, allowedLinkSchemes); ok {
		return `<a href="` + escapeHTML(u) + `" target="_blank" rel="noopener noreferrer nofollow">` + escapeHTML(truncateRunes(disp, maxTextRunes)) + `</a>`
	}
	return escapeHTML(truncateRunes(disp, maxTextRunes))
}

// safeImg renders an <img> tag if src validates, and reports whether it did -
// callers that have nothing sensible to show without an image skip the
// element entirely when this returns false.
func safeImg(src, alt string, extraStyle string) (string, bool) {
	u, ok := sanitizeURL(src, allowedImageSchemes)
	if !ok {
		return "", false
	}
	style := "max-width:100%;display:block;"
	if extraStyle != "" {
		style += extraStyle
	}
	return `<img src="` + escapeHTML(u) + `" alt="` + escapeHTML(truncateRunes(alt, maxTextRunes)) +
		`" loading="lazy" referrerpolicy="no-referrer" style="` + style + `">`, true
}

// truncateRunes shortens s to n runes, adding an ellipsis marker. It exists
// so no single text field can force an unbounded amount of parsing or output.
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
