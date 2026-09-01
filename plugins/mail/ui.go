package mail

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// UIListLimit caps how many messages the inbox renders at once.
const UIListLimit = 200

// RegisterUI mounts the inbox tab. The core strips /ui/mail, so the patterns
// here are relative to it.
//
// The generic event view fills in any route a plugin leaves unclaimed, so the
// two event-shaped routes are claimed as aliases of the mail ones: a tab that
// mixed both views would show the same message two different ways.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	h := &uiHandler{d: d.Normalize()}
	h.tpl, h.tplErr = ui.PluginTemplates(p.Templates())

	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /list", h.list)
	mux.HandleFunc("GET /messages/{id}", h.detail)
	mux.HandleFunc("GET /messages/{id}/body", h.body)
	mux.HandleFunc("DELETE /messages", h.clear)
	mux.HandleFunc("GET /events/{id}", h.detail)
	mux.HandleFunc("DELETE /events", h.clear)
}

type uiHandler struct {
	d      plugin.Deps
	tpl    *template.Template
	tplErr error
}

// inboxFilter is the state of the filter bar.
type inboxFilter struct {
	Search      string
	Provider    string
	Attachments string // "", "1" or "0"
}

// messageRow is one entry of the message list.
type messageRow struct {
	ID          string
	Provider    string
	ReceivedAt  time.Time
	From        string
	To          string
	Subject     string
	Snippet     string
	Attachments int
	HasHTML     bool
	Selected    bool
}

// inboxView is the data behind the whole tab.
type inboxView struct {
	Base      string // "/ui/mail"
	APIBase   string // "/api/v1", wherever the browser reaches the API
	Filter    inboxFilter
	Messages  []messageRow
	Providers []string
	Selected  *messageDetail

	// Info describes this plugin's providers, scoped to the mail tab, so the
	// how-to-test panel and the empty state can offer a command that actually
	// puts a message in, rendered against the ports this instance actually
	// bound.
	Info []plugin.PluginInfo
}

// ListURL is where the list fragment is fetched from.
func (v inboxView) ListURL() string { return v.Base + "/list" }

// ClearURL is what the Clear button deletes.
func (v inboxView) ClearURL() string { return v.Base + "/messages" }

// RefreshURL is ListURL carrying the current filter, so the live SSE refresh
// does not silently widen what the user is looking at.
func (v inboxView) RefreshURL() string {
	q := url.Values{}
	if v.Filter.Search != "" {
		q.Set("search", v.Filter.Search)
	}
	if v.Filter.Provider != "" {
		q.Set("provider", v.Filter.Provider)
	}
	if v.Filter.Attachments != "" {
		q.Set("attachments", v.Filter.Attachments)
	}
	if len(q) == 0 {
		return v.ListURL()
	}
	return v.ListURL() + "?" + q.Encode()
}

// MessageTable renders the list with the shared table component, so the inbox
// looks like every other tab.
func (v inboxView) MessageTable() components.Table {
	t := components.Table{
		Columns: []string{"Time", "From", "Subject", ""},
		Empty:   v.emptyMessage(),
	}
	now := time.Now()
	for _, m := range v.Messages {
		badges := fmt.Sprintf(`<span class="badge badge-info">%s</span>`, template.HTMLEscapeString(m.Provider))
		if m.Attachments > 0 {
			badges += fmt.Sprintf(` <span class="badge badge-muted" title="attachments">%d file%s</span>`,
				m.Attachments, plural(m.Attachments))
		}
		if m.HasHTML {
			badges += ` <span class="badge badge-muted" title="has an HTML part">html</span>`
		}
		subject := m.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		absolute := m.ReceivedAt.Local().Format("2006-01-02 15:04:05.000 MST")
		timeCell := fmt.Sprintf(`<span class="mono" title="%s">%s</span>`,
			template.HTMLEscapeString(absolute), template.HTMLEscapeString(relativeTime(m.ReceivedAt, now)))
		t.Rows = append(t.Rows, components.Row{
			Href:     v.Base + "/messages/" + m.ID,
			Target:   "#detail",
			Selected: m.Selected,
			Cells: []components.Cell{
				{HTML: template.HTML(timeCell), Class: "nowrap"},
				{HTML: template.HTML(`<span>` + template.HTMLEscapeString(components.Truncate(32, m.From)) +
					`</span><br><span class="muted">to ` + template.HTMLEscapeString(components.Truncate(36, m.To)) + `</span>`)},
				{HTML: template.HTML(`<strong>` + template.HTMLEscapeString(components.Truncate(70, subject)) +
					`</strong><br><span class="muted">` + template.HTMLEscapeString(components.Truncate(90, m.Snippet)) + `</span>`)},
				{HTML: template.HTML(badges), Class: "nowrap"},
			},
		})
	}
	return t
}

// emptyMessage tells "nothing has ever arrived" apart from "the filter matched
// nothing", which otherwise look identical and send whoever narrowed the
// filter looking in the wrong place.
func (v inboxView) emptyMessage() string {
	if len(v.Messages) > 0 {
		return ""
	}
	f := v.Filter
	if f.Search != "" || f.Provider != "" || f.Attachments != "" {
		return "No message matches this filter."
	}
	return "No mail captured yet."
}

// relativeTime renders a short "how long ago", falling back to a date once a
// message is old enough that the exact minute stops mattering.
func relativeTime(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return t.Local().Format("15:04:05")
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// HowToTest carries the panel that says how to get mail in here, open when the
// inbox has nothing in it - which is exactly when someone needs it.
func (v inboxView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: len(v.Messages) == 0}
}

// EmptyState is what the detail pane shows with nothing selected. With no mail
// at all it carries the enabled providers, so the panel that tells you how to
// send one is on the tab where you noticed it was empty.
func (v inboxView) EmptyState() components.EmptyState {
	if len(v.Messages) > 0 {
		return components.EmptyState{
			Title:   "Pick a message",
			Message: "Select a message on the left to read its headers, its body and its attachments.",
		}
	}
	return components.EmptyState{
		Title:     "No mail yet",
		Message:   "Point your application's mail client at tommy and whatever it sends lands here, live.",
		Providers: flattenProviders(v.Info),
	}
}

// flattenProviders pulls the providers out of a plugin info list. In the mail
// tab's own scope that list has at most one entry, but this stays correct even
// if that scoping ever widens.
func flattenProviders(info []plugin.PluginInfo) []plugin.ProviderInfo {
	var out []plugin.ProviderInfo
	for _, p := range info {
		out = append(out, p.Providers...)
	}
	return out
}

// bodyView is one of the html / text / raw toggles.
type bodyView struct {
	Key    string
	Label  string
	URL    string
	Active bool
}

// messageDetail is one message opened in the detail pane.
type messageDetail struct {
	Base       string
	APIBase    string
	ID         string
	Provider   string
	Type       string
	ReceivedAt time.Time
	Msg        *Message
	Meta       map[string]any
	Raw        event.Raw
	View       string // "html", "text" or "raw"
}

// Subject is the displayed title.
func (d messageDetail) Subject() string { return d.Msg.Subject }

// Headers reports whether the sender attached any headers of its own.
func (d messageDetail) Headers() bool { return len(d.Msg.Headers) > 0 }

func (d messageDetail) apiMessageURL() string {
	return d.APIBase + "/" + PluginName + "/messages/" + d.ID
}

// HTMLURL is the sandboxed iframe's source: the HTML body, served as HTML by
// the API and never inlined into this page.
func (d messageDetail) HTMLURL() string { return d.apiMessageURL() + "/html" }

// TextURL and RawURL are the same bodies as a plain download.
func (d messageDetail) TextURL() string { return d.apiMessageURL() + "/text" }
func (d messageDetail) RawURL() string  { return d.apiMessageURL() + "/raw" }

// Views are the body toggles, in display order.
func (d messageDetail) Views() []bodyView {
	base := d.Base + "/messages/" + d.ID + "/body?view="
	out := make([]bodyView, 0, 3)
	for _, v := range []struct{ key, label string }{
		{"html", "HTML"},
		{"text", "Text"},
		{"raw", "Raw"},
	} {
		out = append(out, bodyView{Key: v.key, Label: v.label, URL: base + v.key, Active: v.key == d.View})
	}
	return out
}

// HeaderTable is the envelope every mail client shows at the top of a message.
func (d messageDetail) HeaderTable() components.KVTable {
	t := components.KVTable{Caption: "Headers"}
	add := func(key, value string) {
		if value != "" {
			t.Rows = append(t.Rows, components.KV{Key: key, Value: value})
		}
	}
	add("From", d.Msg.From.String())
	add("To", FormatAddressList(d.Msg.To))
	add("Cc", FormatAddressList(d.Msg.Cc))
	add("Bcc", FormatAddressList(d.Msg.Bcc))
	add("Reply-To", FormatAddressList(d.Msg.ReplyTo))
	add("Subject", d.Msg.Subject)
	add("Date", d.ReceivedAt.Local().Format("2006-01-02 15:04:05.000 MST"))
	add("Event", d.ID)
	return t
}

// HeaderExtras lists whatever headers the sender set itself, which is where the
// vendor-specific ones (X-MJ-..., X-SMTPAPI, ...) show up.
func (d messageDetail) HeaderExtras() components.KVTable {
	t := components.KVTable{Caption: "Message headers"}
	for _, k := range d.Msg.Headers.Keys() {
		for _, v := range d.Msg.Headers[k] {
			t.Rows = append(t.Rows, components.KV{Key: k, Value: v})
		}
	}
	return t
}

// AttachmentTable lists the attachments with a streaming download link each.
func (d messageDetail) AttachmentTable() components.Table {
	t := components.Table{
		Caption: "Attachments",
		Columns: []string{"File", "Type", "Size", ""},
		Empty:   "No attachments.",
	}
	for i, a := range d.Msg.Attachments {
		href := fmt.Sprintf("%s/attachments/%d", d.apiMessageURL(), i)
		link := fmt.Sprintf(`<a href="%s" download>%s</a>`,
			template.HTMLEscapeString(href), template.HTMLEscapeString(a.Name()))
		typeCell := fmt.Sprintf(`<span class="badge badge-muted" title="%s">%s</span><br><span class="muted mono">%s</span>`,
			template.HTMLEscapeString(a.ContentType), template.HTMLEscapeString(attachmentKind(a.ContentType)),
			template.HTMLEscapeString(a.ContentType))
		kind := ""
		if a.Inline {
			kind = fmt.Sprintf(`<span class="badge badge-muted" title="embedded in the HTML body, not offered as a download">inline%s</span>`,
				template.HTMLEscapeString(cidSuffix(a.ContentID)))
		} else {
			kind = `<span class="badge badge-muted" title="offered as a download">attachment</span>`
		}
		t.Rows = append(t.Rows, components.Row{Cells: []components.Cell{
			{HTML: template.HTML(link)},
			{HTML: template.HTML(typeCell)},
			{Text: components.BytesHuman(a.Size), Class: "mono nowrap"},
			{HTML: template.HTML(kind), Class: "nowrap"},
		}})
	}
	return t
}

func cidSuffix(cid string) string {
	if cid == "" {
		return ""
	}
	return " cid:" + cid
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// attachmentKind buckets a MIME type into something short enough for a badge.
// The full content type stays visible right next to it, so this is purely a
// scan aid, never the only place the real type is shown.
func attachmentKind(contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.Contains(ct, "pdf"):
		return "pdf"
	case strings.Contains(ct, "zip") || strings.Contains(ct, "compressed") ||
		strings.Contains(ct, "tar") || strings.Contains(ct, "gzip"):
		return "archive"
	case strings.Contains(ct, "csv") || strings.Contains(ct, "spreadsheet") || strings.Contains(ct, "excel"):
		return "sheet"
	case strings.Contains(ct, "word") || strings.Contains(ct, "document"):
		return "doc"
	case strings.HasPrefix(ct, "text/"):
		return "text"
	default:
		return "file"
	}
}

// view builds the whole tab from the store.
func (h *uiHandler) view(r *http.Request) (inboxView, error) {
	shell := ui.ShellFrom(r)
	q := r.URL.Query()
	v := inboxView{
		Base:    UIPrefix,
		APIBase: strings.TrimSuffix(shell.APIBase, "/"),
		Info:    providerInfo(shell),
		Filter: inboxFilter{
			Search:      q.Get("search"),
			Provider:    q.Get("provider"),
			Attachments: attachmentsFilter(q.Get("attachments")),
		},
	}

	events, err := h.d.Store.List(r.Context(), store.Query{
		Plugin:   PluginName,
		Provider: v.Filter.Provider,
		Search:   v.Filter.Search,
		Limit:    UIListLimit,
	})
	if err != nil {
		return v, err
	}

	// The provider dropdown is built from an unfiltered listing, so narrowing
	// never removes the option you would need to widen it again.
	all, err := h.d.Store.List(r.Context(), store.Query{Plugin: PluginName})
	if err != nil {
		return v, err
	}
	v.Providers = providerNames(all)

	for _, e := range events {
		m, ok := MessageOf(e)
		if !ok {
			continue
		}
		if v.Filter.Attachments == "1" && !m.HasAttachments() {
			continue
		}
		if v.Filter.Attachments == "0" && m.HasAttachments() {
			continue
		}
		v.Messages = append(v.Messages, messageRow{
			ID:          string(e.ID),
			Provider:    e.Provider,
			ReceivedAt:  e.ReceivedAt,
			From:        m.From.String(),
			To:          FormatAddressList(m.Recipients()),
			Subject:     m.Subject,
			Snippet:     m.Snippet(),
			Attachments: len(m.Attachments),
			HasHTML:     m.HTML != "",
		})
	}
	return v, nil
}

func attachmentsFilter(v string) string {
	switch v {
	case "1", "0":
		return v
	default:
		return ""
	}
}

func providerNames(events []*event.Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range events {
		if e.Provider != "" && !seen[e.Provider] {
			seen[e.Provider] = true
			out = append(out, e.Provider)
		}
	}
	sort.Strings(out)
	return out
}

// load builds the detail pane for one event.
func (h *uiHandler) load(r *http.Request, id, wantView string) (*messageDetail, error) {
	e, err := h.d.Store.Get(r.Context(), event.ID(id))
	if err != nil {
		return nil, err
	}
	if e.Plugin != PluginName {
		return nil, store.ErrNotFound
	}
	m, ok := MessageOf(e)
	if !ok {
		return nil, store.ErrNotFound
	}
	shell := ui.ShellFrom(r)
	return &messageDetail{
		Base:       UIPrefix,
		APIBase:    strings.TrimSuffix(shell.APIBase, "/"),
		ID:         string(e.ID),
		Provider:   e.Provider,
		Type:       e.Type,
		ReceivedAt: e.ReceivedAt,
		Msg:        m,
		Meta:       e.Meta,
		Raw:        e.Raw,
		View:       defaultView(m, wantView),
	}, nil
}

// defaultView opens on the richest body the message actually has, so a
// text-only message does not greet you with an empty frame.
func defaultView(m *Message, want string) string {
	switch want {
	case "html", "text", "raw":
		return want
	}
	switch {
	case m.HTML != "":
		return "html"
	case m.Text != "":
		return "text"
	default:
		return "raw"
	}
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if id := r.URL.Query().Get("message"); id != "" {
		if d, err := h.load(r, id, r.URL.Query().Get("view")); err == nil {
			h.selectMessage(&v, d)
		}
	}
	h.renderPage(w, r, v)
}

// selectMessage marks the chosen row and hands the detail to the pane.
func (h *uiHandler) selectMessage(v *inboxView, d *messageDetail) {
	v.Selected = d
	for i := range v.Messages {
		v.Messages[i].Selected = v.Messages[i].ID == d.ID
	}
}

func (h *uiHandler) renderPage(w http.ResponseWriter, r *http.Request, v inboxView) {
	body, err := h.render("mail-inbox", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := ui.Render(w, r, "Mail", body); err != nil {
		h.d.Logger.Warn("render mail tab", "err", err)
	}
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.fragment(w, "mail-list", v)
}

func (h *uiHandler) detail(w http.ResponseWriter, r *http.Request) {
	d, err := h.load(r, r.PathValue("id"), r.URL.Query().Get("view"))
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if ui.IsPartial(r) {
		h.fragment(w, "mail-detail", d)
		return
	}
	// A deep link renders the whole tab with the message open, so a message URL
	// can be pasted into a bug report.
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.selectMessage(&v, d)
	h.renderPage(w, r, v)
}

func (h *uiHandler) body(w http.ResponseWriter, r *http.Request) {
	d, err := h.load(r, r.PathValue("id"), r.URL.Query().Get("view"))
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	h.fragment(w, "mail-body", d)
}

func (h *uiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Store.Clear(r.Context(), PluginName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.fragment(w, "mail-list", v)
}

func (h *uiHandler) render(name string, data any) (template.HTML, error) {
	if h.tplErr != nil {
		return "", h.tplErr
	}
	var buf bytes.Buffer
	if err := h.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func (h *uiHandler) fragment(w http.ResponseWriter, name string, data any) {
	body, err := h.render(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// providerInfo pulls this plugin's own entry out of the shell, so the
// how-to-test panel and the empty state can show snippets rendered against the
// ports actually in use.
func providerInfo(shell *ui.Shell) []plugin.PluginInfo {
	for _, p := range shell.Info() {
		if p.Name == PluginName {
			return []plugin.PluginInfo{p}
		}
	}
	return nil
}
