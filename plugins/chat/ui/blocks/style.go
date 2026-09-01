package blocks

import (
	"regexp"
	"strconv"
	"strings"
)

// Every style below is a fixed, package-authored string - never built from
// payload content - and referenced only through inline style="" attributes.
// The seam this package fills forbids emitting a <style> tag, so theming
// comes from the CSS custom properties the shell already defines on :root in
// core/server/ui/static/app.css (--ink, --muted, --line, --panel, --bg,
// --accent, --accent-soft, --warn, --error, --mono): an inline style that
// reads var(--ink) picks up the light or dark value the page is already
// using, with no separate dark-mode branch needed here.
const (
	cardStyle = "border:1px solid var(--line);border-radius:.5rem;padding:.55rem .75rem;background:var(--panel);" +
		"margin:.2rem 0;overflow-wrap:anywhere;"
	colorBarStyle = "border-left:4px solid %s;padding-left:.6rem;"
	dividerStyle  = "border:none;border-top:1px solid var(--line);margin:.5rem 0;"
	headerStyle   = "font-weight:700;font-size:1.05em;margin:.2rem 0;color:var(--ink);"
	textStyle     = "margin:.2rem 0;color:var(--ink);white-space:pre-wrap;overflow-wrap:anywhere;"
	mutedStyle    = "color:var(--muted);font-size:.85em;"
	fieldsStyle   = "display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:.15rem 1rem;margin:.3rem 0;"
	fieldLabel    = "font-weight:600;color:var(--muted);font-size:.82em;"
	fieldValue    = "color:var(--ink);"
	preStyle      = "background:var(--bg);border:1px solid var(--line);border-radius:.35rem;padding:.4rem .5rem;" +
		"overflow-x:auto;margin:.3rem 0;"
	codeStyle   = "background:var(--bg);border:1px solid var(--line);border-radius:.25rem;padding:0 .25rem;"
	buttonStyle = "display:inline-block;border:1px solid var(--line);border-radius:.35rem;padding:.25rem .65rem;" +
		"margin:.15rem .3rem .15rem 0;background:var(--bg);color:var(--ink);font-weight:600;font-size:.85em;" +
		"cursor:default;opacity:.85;"
	buttonPrimary  = "border-color:var(--accent);color:var(--accent);background:var(--accent-soft);"
	buttonDanger   = "border-color:var(--error);color:var(--error);"
	actionsStyle   = "margin:.35rem 0;"
	contextStyle   = "display:flex;flex-wrap:wrap;gap:.4rem;align-items:center;color:var(--muted);font-size:.85em;margin:.3rem 0;"
	columnsStyle   = "display:flex;flex-wrap:wrap;gap:.75rem;margin:.3rem 0;"
	columnStyle    = "flex:1 1 160px;min-width:0;"
	containerStyle = "margin:.3rem 0;"
	factsetStyle   = "display:grid;grid-template-columns:max-content 1fr;gap:.15rem .75rem;margin:.3rem 0;"
	imgWrapStyle   = "margin:.3rem 0;"
	truncNoteStyle = "color:var(--muted);font-size:.8em;font-style:italic;margin:.3rem 0;"
)

// hexColorRe matches a bare or #-prefixed 3/6/8 digit hex color, which is
// what both Slack's attachment "color" and a MessageCard's "themeColor" use
// besides their named aliases.
var hexColorRe = regexp.MustCompile(`^[0-9a-fA-F]{3}$|^[0-9a-fA-F]{6}$|^[0-9a-fA-F]{8}$`)

// sanitizeColor turns an attachment/card color into a CSS color() value,
// falling back to the theme's line color for anything that is not a bare
// hex triplet or one of Slack's named aliases - never passing the raw string
// into the stylesheet, since arbitrary CSS is an injection surface (property
// smuggling, expression(), url()) even though it cannot execute script here.
func sanitizeColor(raw string) string {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "good":
		return "#2eb67d"
	case "warning":
		return "#ecb22e"
	case "danger":
		return "#e01e5a"
	case "":
		return "var(--line)"
	}
	hex := strings.TrimPrefix(raw, "#")
	if hexColorRe.MatchString(hex) {
		return "#" + hex
	}
	return "var(--line)"
}

func truncationNote(kind string, shown, total int) string {
	if total <= shown {
		return ""
	}
	return `<p style="` + truncNoteStyle + `">… ` + escapeHTML(kind) + ` truncated: showing ` +
		strconv.Itoa(shown) + ` of ` + strconv.Itoa(total) + `</p>`
}
