package blocks

import (
	"encoding/json"
	"html/template"
	"strings"
	"unicode/utf8"
)

// renderAdaptiveCard renders an Adaptive Card object
// (chat.FormatTeamsAdaptiveCard), already unwrapped from its Bot Framework
// envelope: {"type":"AdaptiveCard","body":[...],"actions":[...]}. It covers
// TextBlock (weight/size/wrap/isSubtle), FactSet, ColumnSet/Column,
// Container, Image, ActionSet and top-level actions (rendered inert), and
// nested bodies up to maxDepth.
func renderAdaptiveCard(data json.RawMessage) (template.HTML, bool) {
	v, ok := decodeAny(data)
	if !ok {
		return "", false
	}
	m, ok := asMap(v)
	if !ok {
		return "", false
	}

	bud := newBudget()
	var out strings.Builder
	wrote := false

	if body := getSlice(m, "body"); len(body) > 0 {
		if html := renderAdaptiveItems(body, bud, 0); html != "" {
			out.WriteString(html)
			wrote = true
		}
	}
	if actions := getSlice(m, "actions"); len(actions) > 0 {
		if html := renderAdaptiveActions(actions, bud); html != "" {
			out.WriteString(html)
			wrote = true
		}
	}

	if !wrote {
		return "", false
	}
	return template.HTML(`<div style="` + cardStyle + `">` + out.String() + `</div>`), true
}

// renderAdaptiveItems renders a body/Container/Column items array. depth
// counts container nesting; past maxDepth it renders a note instead of
// recursing further, which is what keeps a pathologically nested card from
// blowing the stack.
func renderAdaptiveItems(items []any, bud *budget, depth int) string {
	if depth > maxDepth {
		return `<p style="` + truncNoteStyle + `">… content nested too deeply, not shown</p>`
	}
	var b strings.Builder
	processed := 0
	for _, it := range items {
		if !bud.take() {
			break
		}
		processed++
		im, ok := asMap(it)
		if !ok {
			continue
		}
		b.WriteString(renderAdaptiveElement(im, bud, depth))
	}
	b.WriteString(truncationNote("items", processed, len(items)))
	return b.String()
}

func renderAdaptiveElement(im map[string]any, bud *budget, depth int) string {
	switch getStr(im, "type") {
	case "TextBlock":
		return renderTextBlockElem(im)
	case "FactSet":
		return renderFactSet(im, bud)
	case "ColumnSet":
		return renderColumnSet(im, bud, depth)
	case "Container":
		return renderContainerElem(im, bud, depth)
	case "Image":
		return renderAdaptiveImage(im)
	case "ActionSet":
		return renderAdaptiveActions(getSlice(im, "actions"), bud)
	default:
		return ""
	}
}

func textBlockStyle(im map[string]any) string {
	style := "margin:.2rem 0;color:var(--ink);overflow-wrap:anywhere;white-space:normal;"
	switch getStr(im, "size") {
	case "Small":
		style += "font-size:.85em;"
	case "Medium":
		style += "font-size:1.1em;"
	case "Large":
		style += "font-size:1.25em;"
	case "ExtraLarge":
		style += "font-size:1.5em;"
	}
	switch getStr(im, "weight") {
	case "Bolder":
		style += "font-weight:700;"
	case "Lighter":
		style += "font-weight:300;"
	}
	if getBool(im, "isSubtle") {
		style += "color:var(--muted);"
	}
	if b, ok := im["wrap"].(bool); ok && !b {
		style += "white-space:nowrap;overflow:hidden;text-overflow:ellipsis;"
	}
	return style
}

func renderTextBlockElem(im map[string]any) string {
	return `<div style="` + textBlockStyle(im) + `">` + renderAdaptiveText(getStr(im, "text")) + `</div>`
}

func renderFactSet(im map[string]any, bud *budget) string {
	facts := getSlice(im, "facts")
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div style="` + factsetStyle + `">`)
	shown := 0
	for _, f := range facts {
		if !bud.take() {
			break
		}
		shown++
		fm, ok := asMap(f)
		if !ok {
			continue
		}
		b.WriteString(`<div style="` + fieldLabel + `">` + escapeHTML(truncateRunes(getStr(fm, "title"), maxTextRunes)) + `</div>`)
		b.WriteString(`<div style="` + fieldValue + `">` + renderAdaptiveText(getStr(fm, "value")) + `</div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(truncationNote("facts", shown, len(facts)))
	return b.String()
}

func renderColumnSet(im map[string]any, bud *budget, depth int) string {
	if depth+1 > maxDepth {
		return `<p style="` + truncNoteStyle + `">… content nested too deeply, not shown</p>`
	}
	columns := getSlice(im, "columns")
	var b strings.Builder
	b.WriteString(`<div style="` + columnsStyle + `">`)
	shown := 0
	for _, c := range columns {
		if !bud.take() {
			break
		}
		shown++
		cm, ok := asMap(c)
		if !ok {
			continue
		}
		b.WriteString(`<div style="` + columnStyle + `">`)
		b.WriteString(renderAdaptiveItems(getSlice(cm, "items"), bud, depth+1))
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(truncationNote("columns", shown, len(columns)))
	return b.String()
}

func renderContainerElem(im map[string]any, bud *budget, depth int) string {
	if depth+1 > maxDepth {
		return `<p style="` + truncNoteStyle + `">… content nested too deeply, not shown</p>`
	}
	return `<div style="` + containerStyle + `">` + renderAdaptiveItems(getSlice(im, "items"), bud, depth+1) + `</div>`
}

func renderAdaptiveImage(im map[string]any) string {
	size := "max-height:320px;"
	switch getStr(im, "size") {
	case "Small":
		size = "max-height:40px;"
	case "Medium":
		size = "max-height:80px;"
	case "Large":
		size = "max-height:160px;"
	}
	img, ok := safeImg(getStr(im, "url"), getStr(im, "altText"), "border-radius:.35rem;"+size)
	if !ok {
		return ""
	}
	return `<div style="` + imgWrapStyle + `">` + img + `</div>`
}

func renderAdaptiveActions(actions []any, bud *budget) string {
	if len(actions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div style="` + actionsStyle + `">`)
	shown := 0
	for _, a := range actions {
		if !bud.take() {
			break
		}
		shown++
		am, ok := asMap(a)
		if !ok {
			continue
		}
		title := getStr(am, "title")
		if strings.TrimSpace(title) == "" {
			title = getStr(am, "type")
		}
		if strings.TrimSpace(title) == "" {
			title = "Action"
		}
		style := buttonStyle
		switch getStr(am, "style") {
		case "positive":
			style += buttonPrimary
		case "destructive":
			style += buttonDanger
		}
		b.WriteString(`<span style="` + style + `" title="Inert in this preview">` + escapeHTML(truncateRunes(title, maxTextRunes)) + `</span>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(truncationNote("actions", shown, len(actions)))
	return b.String()
}

// renderAdaptiveText renders Adaptive Card's own small markdown subset:
// **bold**, *italic*, `code` and [label](url), with a bare "\n" as a line
// break. It is deliberately separate from renderMrkdwn - Slack and Adaptive
// Card markdown use different delimiters for bold - but shares the same
// discipline: a single forward pass, every literal byte escaped, every link
// URL validated before it becomes an href.
func renderAdaptiveText(s string) string {
	s = truncateRunes(s, maxTextRunes)
	var out strings.Builder
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '\n':
			out.WriteString("<br>")
			i++
			continue
		case c == '*' && i+1 < n && s[i+1] == '*':
			if end := strings.Index(s[i+2:], "**"); end > 0 {
				out.WriteString("<strong>" + escapeHTML(s[i+2:i+2+end]) + "</strong>")
				i += 2 + end + 2
				continue
			}
		case c == '*':
			if end := spanEnd(s, i+1, '*'); end > 0 {
				out.WriteString("<em>" + escapeHTML(s[i+1:end]) + "</em>")
				i = end + 1
				continue
			}
		case c == '`':
			if end := spanEnd(s, i+1, '`'); end > 0 {
				out.WriteString(`<code style="` + codeStyle + `">` + escapeHTML(s[i+1:end]) + `</code>`)
				i = end + 1
				continue
			}
		case c == '[':
			if close := strings.IndexByte(s[i:], ']'); close > 0 {
				labelEnd := i + close
				if labelEnd+1 < n && s[labelEnd+1] == '(' {
					if paren := strings.IndexByte(s[labelEnd+2:], ')'); paren >= 0 {
						label := s[i+1 : labelEnd]
						target := s[labelEnd+2 : labelEnd+2+paren]
						out.WriteString(safeLink(target, label))
						i = labelEnd + 2 + paren + 1
						continue
					}
				}
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
