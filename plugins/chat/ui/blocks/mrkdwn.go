package blocks

import (
	"strings"
	"unicode/utf8"
)

// renderMrkdwn renders a reasonable subset of Slack's mrkdwn as safe, already
// html-escaped markup: *bold*, _italic_, ~strike~, `code`, ```code blocks```,
// and <url|label> links plus <@U123>/<#C123>/<!here> mentions. It is a single
// forward pass with no backtracking, so it never recurses and every literal
// character it emits goes through escapeHTML - there is no path from input
// bytes to unescaped output.
//
// The parser is intentionally non-nested (the text inside *bold* is escaped,
// not re-parsed for _italic_ inside it): Slack's own mrkdwn does not nest
// either, and it keeps this function trivially bounded.
func renderMrkdwn(s string) string {
	s = truncateRunes(s, maxTextRunes)
	var out strings.Builder
	i := 0
	n := len(s)
	for i < n {
		c := s[i]

		if c == '`' && strings.HasPrefix(s[i:], "```") {
			if end := strings.Index(s[i+3:], "```"); end >= 0 {
				content := strings.Trim(s[i+3:i+3+end], "\n")
				out.WriteString(`<pre style="` + preStyle + `"><code>`)
				out.WriteString(escapeHTML(content))
				out.WriteString(`</code></pre>`)
				i += 3 + end + 3
				continue
			}
		}

		switch c {
		case '`':
			if end := spanEnd(s, i+1, '`'); end > 0 {
				out.WriteString(`<code style="` + codeStyle + `">`)
				out.WriteString(escapeHTML(s[i+1 : end]))
				out.WriteString(`</code>`)
				i = end + 1
				continue
			}
		case '<':
			if end := strings.IndexByte(s[i:], '>'); end > 0 {
				token := s[i+1 : i+end]
				if !strings.ContainsAny(token, "<\n") {
					if html, ok := renderLinkOrMention(token); ok {
						out.WriteString(html)
						i += end + 1
						continue
					}
				}
			}
		case '*':
			if end := spanEnd(s, i+1, '*'); end > 0 {
				out.WriteString(`<strong>`)
				out.WriteString(escapeHTML(s[i+1 : end]))
				out.WriteString(`</strong>`)
				i = end + 1
				continue
			}
		case '_':
			if end := spanEnd(s, i+1, '_'); end > 0 {
				out.WriteString(`<em>`)
				out.WriteString(escapeHTML(s[i+1 : end]))
				out.WriteString(`</em>`)
				i = end + 1
				continue
			}
		case '~':
			if end := spanEnd(s, i+1, '~'); end > 0 {
				out.WriteString(`<s>`)
				out.WriteString(escapeHTML(s[i+1 : end]))
				out.WriteString(`</s>`)
				i = end + 1
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			break
		}
		out.WriteString(escapeHTML(string(r)))
		i += size
	}
	return out.String()
}

// spanEnd finds the closing delim for an emphasis span opened at start-1,
// requiring non-empty content that does not cross a line break. It returns
// -1 when there is no valid close, in which case the opening delimiter is
// emitted as a literal character instead.
func spanEnd(s string, start int, delim byte) int {
	rel := strings.IndexByte(s[start:], delim)
	if rel <= 0 {
		return -1
	}
	end := start + rel
	if strings.ContainsRune(s[start:end], '\n') {
		return -1
	}
	return end
}

// renderLinkOrMention renders the content of a <...> token: a link
// (optionally with a |label), or a user/channel/special mention. It returns
// false for a token that is neither, so the caller falls back to treating
// the angle brackets as literal text.
func renderLinkOrMention(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	target, label := token, ""
	if idx := strings.IndexByte(token, '|'); idx >= 0 {
		target, label = token[:idx], token[idx+1:]
	}

	switch {
	case strings.HasPrefix(target, "@"):
		disp := label
		if disp == "" {
			disp = target
		} else {
			disp = "@" + label
		}
		return `<span class="chat-mention">` + escapeHTML(disp) + `</span>`, true
	case strings.HasPrefix(target, "#"):
		disp := label
		if disp == "" {
			disp = target
		} else {
			disp = "#" + label
		}
		return `<span class="chat-mention">` + escapeHTML(disp) + `</span>`, true
	case strings.HasPrefix(target, "!"):
		name := strings.TrimPrefix(target, "!")
		disp := label
		if disp == "" {
			disp = "@" + name
		}
		return `<span class="chat-mention">` + escapeHTML(disp) + `</span>`, true
	default:
		u, ok := sanitizeURL(target, allowedLinkSchemes)
		if !ok {
			return "", false
		}
		disp := label
		if disp == "" {
			disp = target
		}
		return `<a href="` + escapeHTML(u) + `" target="_blank" rel="noopener noreferrer nofollow">` + escapeHTML(disp) + `</a>`, true
	}
}
