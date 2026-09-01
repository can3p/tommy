package blocks

import (
	"encoding/json"
	"html/template"
	"strings"
)

// renderMessageCard renders an O365 connector MessageCard object
// (chat.FormatTeamsMessageCard): {"@type":"MessageCard","themeColor":"...",
// "title":"...","text":"...","sections":[...],"potentialAction":[...]}. It
// covers title, text, themeColor as a color bar, sections
// (activityTitle/activitySubtitle/activityImage/facts/text) and
// potentialAction rendered as inert buttons.
func renderMessageCard(data json.RawMessage) (template.HTML, bool) {
	v, ok := decodeAny(data)
	if !ok {
		return "", false
	}
	m, ok := asMap(v)
	if !ok {
		return "", false
	}

	bud := newBudget()
	wrote := false
	var out strings.Builder
	style := cardStyle + strings.Replace(colorBarStyle, "%s", sanitizeColor(getStr(m, "themeColor")), 1)
	out.WriteString(`<div style="` + style + `">`)

	if title := getStr(m, "title"); strings.TrimSpace(title) != "" {
		out.WriteString(`<div style="` + headerStyle + `">` + renderMrkdwn(title) + `</div>`)
		wrote = true
	}
	if text := getStr(m, "text"); strings.TrimSpace(text) != "" {
		out.WriteString(`<div style="` + textStyle + `">` + renderMrkdwn(text) + `</div>`)
		wrote = true
	}

	sections := getSlice(m, "sections")
	shownSections := 0
	for _, s := range sections {
		if !bud.take() {
			break
		}
		shownSections++
		sm, ok := asMap(s)
		if !ok {
			continue
		}
		if html, ok := renderMessageCardSection(sm, bud); ok {
			out.WriteString(html)
			wrote = true
		}
	}
	out.WriteString(truncationNote("sections", shownSections, len(sections)))

	if actions := getSlice(m, "potentialAction"); len(actions) > 0 {
		var ab strings.Builder
		ab.WriteString(`<div style="` + actionsStyle + `">`)
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
			ab.WriteString(renderPotentialAction(am))
		}
		ab.WriteString(`</div>`)
		ab.WriteString(truncationNote("actions", shown, len(actions)))
		out.WriteString(ab.String())
		wrote = true
	}

	out.WriteString(`</div>`)
	if !wrote {
		return "", false
	}
	return template.HTML(out.String()), true
}

func renderMessageCardSection(sm map[string]any, bud *budget) (string, bool) {
	var b strings.Builder
	wrote := false
	b.WriteString(`<div style="border-top:1px solid var(--line);padding-top:.4rem;margin-top:.4rem;">`)

	if img, ok := safeImg(getStr(sm, "activityImage"), "", "height:32px;width:32px;border-radius:50%;float:left;margin-right:.5rem;"); ok {
		b.WriteString(img)
		wrote = true
	}
	if at := getStr(sm, "activityTitle"); strings.TrimSpace(at) != "" {
		b.WriteString(`<div style="font-weight:700;">` + renderMrkdwn(at) + `</div>`)
		wrote = true
	}
	if as := getStr(sm, "activitySubtitle"); strings.TrimSpace(as) != "" {
		b.WriteString(`<div style="` + mutedStyle + `">` + renderMrkdwn(as) + `</div>`)
		wrote = true
	}
	if txt := getStr(sm, "text"); strings.TrimSpace(txt) != "" {
		b.WriteString(`<div style="` + textStyle + `clear:both;">` + renderMrkdwn(txt) + `</div>`)
		wrote = true
	}
	if facts := getSlice(sm, "facts"); len(facts) > 0 {
		b.WriteString(`<div style="` + factsetStyle + `clear:both;">`)
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
			b.WriteString(`<div style="` + fieldLabel + `">` + escapeHTML(truncateRunes(getStr(fm, "name"), maxTextRunes)) + `</div>`)
			b.WriteString(`<div style="` + fieldValue + `">` + renderMrkdwn(getStr(fm, "value")) + `</div>`)
		}
		b.WriteString(`</div>`)
		b.WriteString(truncationNote("facts", shown, len(facts)))
		wrote = true
	}

	b.WriteString(`</div>`)
	if !wrote {
		return "", false
	}
	return b.String(), true
}

func renderPotentialAction(am map[string]any) string {
	name := getStr(am, "name")
	if strings.TrimSpace(name) == "" {
		name = getStr(am, "@type")
	}
	if strings.TrimSpace(name) == "" {
		name = "Action"
	}
	return `<span style="` + buttonStyle + `" title="Inert in this preview">` + escapeHTML(truncateRunes(name, maxTextRunes)) + `</span>`
}
