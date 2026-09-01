package blocks

import (
	"encoding/json"
	"html/template"
	"strings"
	"time"
)

// renderSlackAttachments renders Slack's legacy "attachments" array
// (chat.FormatSlackAttachments): [{"color":"#36a64f","fields":[...]}, ...].
// It covers the color bar, pretext, author (name/link/icon), title(+link),
// text, fields (title/value, "short" as a layout hint), footer(+icon+ts) and
// image_url/thumb_url.
func renderSlackAttachments(data json.RawMessage) (template.HTML, bool) {
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
		out.WriteString(renderAttachment(m, bud))
		rendered++
	}
	if rendered == 0 {
		return "", false
	}
	out.WriteString(truncationNote("attachments", processed, len(arr)))
	return template.HTML(out.String()), true
}

func renderAttachment(m map[string]any, bud *budget) string {
	var b strings.Builder
	style := cardStyle + strings.Replace(colorBarStyle, "%s", sanitizeColor(getStr(m, "color")), 1)
	b.WriteString(`<div style="` + style + `">`)

	if name := getStr(m, "author_name"); strings.TrimSpace(name) != "" {
		b.WriteString(`<div style="` + mutedStyle + ` margin-bottom:.2rem;">`)
		if icon := getStr(m, "author_icon"); icon != "" {
			if html, ok := safeImg(icon, "", "height:16px;width:16px;border-radius:50%;vertical-align:middle;margin-right:.3rem;"); ok {
				b.WriteString(html)
			}
		}
		if link := getStr(m, "author_link"); link != "" {
			b.WriteString(safeLink(link, name))
		} else {
			b.WriteString(escapeHTML(truncateRunes(name, maxTextRunes)))
		}
		b.WriteString(`</div>`)
	}

	if pretext := getStr(m, "pretext"); strings.TrimSpace(pretext) != "" {
		b.WriteString(`<div style="` + textStyle + `">` + renderMrkdwn(pretext) + `</div>`)
	}

	if title := getStr(m, "title"); strings.TrimSpace(title) != "" {
		b.WriteString(`<div style="font-weight:700;margin:.2rem 0;">`)
		if link := getStr(m, "title_link"); link != "" {
			b.WriteString(safeLink(link, title))
		} else {
			b.WriteString(escapeHTML(truncateRunes(title, maxTextRunes)))
		}
		b.WriteString(`</div>`)
	}

	if text := getStr(m, "text"); strings.TrimSpace(text) != "" {
		b.WriteString(`<div style="` + textStyle + `">` + renderMrkdwn(text) + `</div>`)
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
			b.WriteString(`<div>`)
			if title := getStr(fm, "title"); strings.TrimSpace(title) != "" {
				b.WriteString(`<div style="` + fieldLabel + `">` + escapeHTML(truncateRunes(title, maxTextRunes)) + `</div>`)
			}
			b.WriteString(`<div style="` + fieldValue + `">` + renderMrkdwn(getStr(fm, "value")) + `</div>`)
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div>`)
		b.WriteString(truncationNote("fields", shown, len(fields)))
	}

	if img, ok := safeImg(getStr(m, "image_url"), "", "border-radius:.35rem;max-height:360px;margin-top:.3rem;"); ok {
		b.WriteString(`<div>` + img + `</div>`)
	}
	if thumb, ok := safeImg(getStr(m, "thumb_url"), "", "border-radius:.35rem;max-height:80px;margin-top:.3rem;"); ok {
		b.WriteString(`<div>` + thumb + `</div>`)
	}

	if footer := getStr(m, "footer"); strings.TrimSpace(footer) != "" {
		b.WriteString(`<div style="` + mutedStyle + ` margin-top:.3rem;">`)
		if icon := getStr(m, "footer_icon"); icon != "" {
			if html, ok := safeImg(icon, "", "height:14px;width:14px;border-radius:.2rem;vertical-align:middle;margin-right:.3rem;"); ok {
				b.WriteString(html)
			}
		}
		b.WriteString(escapeHTML(truncateRunes(footer, maxTextRunes)))
		if ts, ok := getNumber(m, "ts"); ok && ts > 0 {
			b.WriteString(` &middot; ` + escapeHTML(time.Unix(int64(ts), 0).UTC().Format("2006-01-02 15:04:05 MST")))
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`</div>`)
	return b.String()
}
