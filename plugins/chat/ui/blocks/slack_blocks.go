package blocks

import (
	"encoding/json"
	"html/template"
	"strings"
)

// renderSlackBlocks renders a Block Kit "blocks" array
// (chat.FormatSlackBlocks): [{"type":"section",...}, ...]. It covers
// section (text, fields, accessory), header, divider, context, actions
// (buttons rendered inert) and image. Anything else - input elements, select
// menus, rich_text, video - is skipped rather than guessed at.
func renderSlackBlocks(data json.RawMessage) (template.HTML, bool) {
	v, ok := decodeAny(data)
	if !ok {
		return "", false
	}
	arr, ok := asSlice(v)
	if !ok {
		return "", false
	}

	bud := newBudget()
	var out strings.Builder
	processed, rendered := 0, 0
	for _, item := range arr {
		if !bud.take() {
			break
		}
		processed++
		m, ok := asMap(item)
		if !ok {
			continue
		}
		if html, ok := renderBlock(m, bud); ok {
			out.WriteString(html)
			rendered++
		}
	}
	if rendered == 0 {
		return "", false
	}

	var wrap strings.Builder
	wrap.WriteString(`<div style="` + cardStyle + `">`)
	wrap.WriteString(out.String())
	wrap.WriteString(truncationNote("blocks", processed, len(arr)))
	wrap.WriteString(`</div>`)
	return template.HTML(wrap.String()), true
}

func renderBlock(m map[string]any, bud *budget) (string, bool) {
	switch getStr(m, "type") {
	case "section":
		return renderSection(m, bud), true
	case "header":
		return renderHeader(m), true
	case "divider":
		return `<hr style="` + dividerStyle + `">`, true
	case "context":
		return renderContext(m, bud), true
	case "actions":
		return renderActions(m, bud), true
	case "image":
		return renderImageBlock(m)
	default:
		return "", false
	}
}

func renderSection(m map[string]any, bud *budget) string {
	var b strings.Builder
	b.WriteString(`<div style="margin:.3rem 0;">`)
	if text := getMap(m, "text"); text != nil {
		b.WriteString(`<div style="` + textStyle + `">` + renderTextObj(text) + `</div>`)
	}
	if fields := getSlice(m, "fields"); len(fields) > 0 {
		b.WriteString(`<div style="` + fieldsStyle + `">`)
		shown := 0
		for _, f := range fields {
			if !bud.take() {
				break
			}
			shown++
			fm, ok := asMap(f)
			if !ok {
				continue
			}
			b.WriteString(`<div style="` + fieldValue + `">` + renderTextObj(fm) + `</div>`)
		}
		b.WriteString(`</div>`)
		b.WriteString(truncationNote("fields", shown, len(fields)))
	}
	if acc := getMap(m, "accessory"); acc != nil {
		if html, ok := renderAccessory(acc); ok {
			b.WriteString(`<div style="margin-top:.3rem;">` + html + `</div>`)
		}
	}
	b.WriteString(`</div>`)
	return b.String()
}

func renderHeader(m map[string]any) string {
	return `<div style="` + headerStyle + `">` + renderTextObj(getMap(m, "text")) + `</div>`
}

func renderContext(m map[string]any, bud *budget) string {
	elements := getSlice(m, "elements")
	var b strings.Builder
	b.WriteString(`<div style="` + contextStyle + `">`)
	shown := 0
	for _, e := range elements {
		if !bud.take() {
			break
		}
		shown++
		em, ok := asMap(e)
		if !ok {
			continue
		}
		if getStr(em, "type") == "image" {
			if html, ok := safeImg(getStr(em, "image_url"), getStr(em, "alt_text"), "max-height:20px;max-width:20px;border-radius:.2rem;vertical-align:middle;"); ok {
				b.WriteString(html)
			}
			continue
		}
		b.WriteString(`<span>` + renderTextObj(em) + `</span>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(truncationNote("context elements", shown, len(elements)))
	return b.String()
}

func renderActions(m map[string]any, bud *budget) string {
	elements := getSlice(m, "elements")
	var b strings.Builder
	b.WriteString(`<div style="` + actionsStyle + `">`)
	shown := 0
	for _, e := range elements {
		if !bud.take() {
			break
		}
		shown++
		em, ok := asMap(e)
		if !ok {
			continue
		}
		if html, ok := renderAccessory(em); ok {
			b.WriteString(html)
			continue
		}
		label := renderTextObj(getMap(em, "text"))
		if strings.TrimSpace(label) == "" {
			label = escapeHTML(truncateRunes(getStr(em, "type"), maxTextRunes))
		}
		b.WriteString(`<span style="` + buttonStyle + `" title="Inert in this preview">` + label + `</span>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(truncationNote("actions", shown, len(elements)))
	return b.String()
}

// renderAccessory renders a section's accessory or an actions/element entry
// that is an image or a button, the two Block Kit elements common enough to
// be worth a dedicated look. Anything else reports false so the caller can
// fall back to a generic inert label.
func renderAccessory(em map[string]any) (string, bool) {
	switch getStr(em, "type") {
	case "image":
		return renderImageBlock(em)
	case "button":
		return renderButton(em), true
	default:
		return "", false
	}
}

func renderButton(em map[string]any) string {
	label := renderTextObj(getMap(em, "text"))
	if strings.TrimSpace(label) == "" {
		label = escapeHTML(truncateRunes(getStr(em, "value"), maxTextRunes))
	}
	if strings.TrimSpace(label) == "" {
		label = "Button"
	}
	style := buttonStyle
	switch getStr(em, "style") {
	case "primary":
		style += buttonPrimary
	case "danger":
		style += buttonDanger
	}
	return `<span style="` + style + `" title="Buttons are inert in this preview">` + label + `</span>`
}

func renderImageBlock(m map[string]any) (string, bool) {
	img, ok := safeImg(getStr(m, "image_url"), getStr(m, "alt_text"), "border-radius:.35rem;max-height:360px;")
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString(`<div style="` + imgWrapStyle + `">`)
	if title := getMap(m, "title"); title != nil {
		if label := renderTextObj(title); strings.TrimSpace(label) != "" {
			b.WriteString(`<div style="` + mutedStyle + `">` + label + `</div>`)
		}
	}
	b.WriteString(img)
	b.WriteString(`</div>`)
	return b.String(), true
}

// renderTextObj renders a Slack "text object" - {"type":"mrkdwn"|"plain_text",
// "text":"..."} - dispatching mrkdwn through renderMrkdwn and everything else
// through a plain escape. A missing or malformed object (nil map, wrong
// types) renders as empty rather than panicking.
func renderTextObj(m map[string]any) string {
	text := getStr(m, "text")
	if getStr(m, "type") == "mrkdwn" {
		return renderMrkdwn(text)
	}
	return escapeHTML(truncateRunes(text, maxTextRunes))
}
