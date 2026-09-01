package sms

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/can3p/tommy/core/plugin"
	coreui "github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// UIBase is where the tab is mounted once the core has taken its prefix off.
const UIBase = coreui.Prefix + "/" + Name

// ListLimit caps how many messages one render of the tab pulls out of the store.
const ListLimit = 500

// uiHandler serves the SMS tab: a conversation list on the left, the selected
// thread as bubbles on the right.
type uiHandler struct {
	plugin *Plugin
	deps   plugin.Deps
	tpl    *template.Template
	tplErr error
}

func newUIHandler(p *Plugin, d plugin.Deps) *uiHandler {
	h := &uiHandler{plugin: p, deps: d}
	h.tpl, h.tplErr = coreui.PluginTemplates(p.Templates(), "*.html")
	return h
}

func (h *uiHandler) mount(mux plugin.Mux) {
	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /list", h.list)
	mux.HandleFunc("GET /conversations/{key}", h.thread)
	mux.HandleFunc("DELETE /events", h.clear)
	// GET /events/{id} is deliberately left to the core's generic view, so an
	// event id from the API or the overview still opens a raw inspector here.
}

// tabView is everything the tab template renders.
type tabView struct {
	Base    string
	UIBase  string
	APIBase string
	// StreamEvent is the SSE frame name the tab refreshes on.
	StreamEvent string

	Search        string
	Conversations []convView
	Selected      *threadView
	Total         int

	// Providers feeds the empty state, carrying snippets already rendered
	// against the ports this instance actually bound.
	Providers []plugin.ProviderInfo
}

// ListURL is where the conversation list fragment is refetched from.
func (v tabView) ListURL() string {
	if v.Search == "" {
		return v.Base + "/list"
	}
	return v.Base + "/list?search=" + url.QueryEscape(v.Search)
}

// HasMessages reports whether anything has been captured at all.
func (v tabView) HasMessages() bool { return v.Total > 0 }

type convView struct {
	Key      string
	URL      string
	Title    string
	Local    string
	Preview  string
	At       time.Time
	Count    int
	Selected bool
	Badges   []components.Badge
}

type threadView struct {
	Key      string
	URL      string
	Peer     string
	Local    string
	Messages []bubbleView
}

type bubbleView struct {
	ID       string
	EventURL string
	At       time.Time
	Outbound bool
	// Body is untrusted text. It is rendered through html/template as a plain
	// string and never as template.HTML.
	Body     string
	Provider string
	Badges   []components.Badge
	Media    []MediaRef
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("conversation"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, v)
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("conversation"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "sms-list", v)
}

func (h *uiHandler) thread(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	v, err := h.view(r, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v.Selected == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	if !coreui.IsPartial(r) {
		// A deep link renders the whole tab with the thread open, so a
		// conversation URL can be pasted into a bug report.
		h.renderPage(w, r, v)
		return
	}
	h.writeFragment(w, "sms-thread", v)
}

func (h *uiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Store.Clear(r.Context(), Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, err := h.view(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The whole tab comes back, because clearing empties the thread pane too.
	h.writeFragment(w, "sms-tab", v)
}

func (h *uiHandler) view(r *http.Request, selectedKey string) (tabView, error) {
	shell := coreui.ShellFrom(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))

	events, err := h.deps.Store.List(r.Context(), store.Query{
		Plugin: Name,
		Search: search,
		Limit:  ListLimit,
	})
	if err != nil {
		return tabView{}, err
	}

	v := tabView{
		Base:        UIBase,
		UIBase:      shell.UIBase,
		APIBase:     APIBase,
		StreamEvent: EventType,
		Search:      search,
		Providers:   h.providerInfo(coreui.ShellFrom(r)),
	}

	convs := Conversations(events)
	for _, c := range convs {
		v.Total += c.Count()
	}
	selected := FindConversation(convs, selectedKey)
	for _, c := range convs {
		v.Conversations = append(v.Conversations, h.convView(c, c == selected))
	}
	if selected != nil {
		thread := h.threadView(selected)
		v.Selected = &thread
	}
	return v, nil
}

func (h *uiHandler) convView(c *Conversation, selected bool) convView {
	preview := ""
	if last := c.Last(); last != nil {
		preview = last.Body
		if preview == "" && last.IsMMS() {
			preview = "(media only)"
		}
		preview = components.Truncate(80, singleLine(preview))
	}
	var badges []components.Badge
	if last := c.Last(); last != nil {
		badges = append(badges, components.Badge{Label: string(last.Status), Tone: last.Status.Tone()})
	}
	return convView{
		Key:      c.Key,
		URL:      UIBase + "/conversations/" + c.Key,
		Title:    c.Title(),
		Local:    c.Local,
		Preview:  preview,
		At:       c.Latest,
		Count:    c.Count(),
		Selected: selected,
		Badges:   badges,
	}
}

func (h *uiHandler) threadView(c *Conversation) threadView {
	t := threadView{
		Key:   c.Key,
		URL:   UIBase + "/conversations/" + c.Key,
		Peer:  c.Peer,
		Local: c.Local,
	}
	for _, item := range c.Items {
		t.Messages = append(t.Messages, bubbleView{
			ID:       string(item.Event.ID),
			EventURL: UIBase + "/events/" + string(item.Event.ID),
			At:       item.Event.ReceivedAt,
			Outbound: item.Message.Direction != Inbound,
			Body:     item.Message.Body,
			Provider: item.Event.Provider,
			Badges:   messageBadges(item.Message),
			Media:    MediaRefs(APIBase, item.Event.ID, item.Message),
		})
	}
	return t
}

// messageBadges is the badge row under a bubble. The segment badge is the whole
// reason this tab exists rather than a generic list, so it is always present.
func messageBadges(m *Message) []components.Badge {
	seg := m.Segments
	tone := "muted"
	if seg.Count > 1 {
		tone = "warn"
	}
	label := strconv.Itoa(seg.Count) + " seg"
	if seg.Count != 1 {
		label = strconv.Itoa(seg.Count) + " segs"
	}
	badges := []components.Badge{
		{
			Label: label,
			Tone:  tone,
			Title: strconv.Itoa(seg.Units) + " of " + strconv.Itoa(seg.Capacity*seg.Count) +
				" units used, " + strconv.Itoa(seg.Remaining) + " free in the last segment",
		},
		{
			Label: string(seg.Encoding),
			Tone:  encodingTone(seg.Encoding),
			Title: encodingTitle(seg.Encoding),
		},
		{Label: string(m.Status), Tone: m.Status.Tone()},
	}
	if m.IsMMS() {
		badges = append(badges, components.Badge{
			Label: "MMS",
			Tone:  "info",
			Title: strconv.Itoa(len(m.Media)) + " attachment(s)",
		})
	}
	return badges
}

func encodingTone(e Encoding) string {
	if e == UCS2 {
		return "warn"
	}
	return "muted"
}

func encodingTitle(e Encoding) string {
	if e == UCS2 {
		return "A character outside GSM-7 forced the whole message to UCS-2: 70 characters per segment, 67 when concatenated"
	}
	return "The GSM-7 default alphabet: 160 characters per segment, 153 when concatenated"
}

// providerInfo describes this plugin's providers for the empty state, taken
// from the shell so the snippets carry the ports actually in use. It falls back
// to the plugin's own list, without snippets, if the shell has no registry -
// which happens only in a handler tested in isolation.
func (h *uiHandler) providerInfo(shell *coreui.Shell) []plugin.ProviderInfo {
	for _, p := range shell.Info() {
		if p.Name == Name {
			return p.Providers
		}
	}

	var out []plugin.ProviderInfo
	for _, prov := range h.plugin.Providers() {
		_, listener := prov.(plugin.ListenerProvider)
		endpoints := prov.Endpoints()
		if endpoints == nil {
			endpoints = []plugin.Endpoint{}
		}
		out = append(out, plugin.ProviderInfo{
			Name:        prov.Name(),
			Plugin:      Name,
			Description: prov.Description(),
			Enabled:     true,
			Listener:    listener,
			Endpoints:   endpoints,
		})
	}
	return out
}

func (h *uiHandler) renderPage(w http.ResponseWriter, r *http.Request, v tabView) {
	body, err := h.render("sms-tab", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "SMS", body); err != nil {
		h.deps.Logger.Warn("render sms tab", "err", err)
	}
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

func (h *uiHandler) writeFragment(w http.ResponseWriter, name string, data any) {
	body, err := h.render(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\r", " ")
}
