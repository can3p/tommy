package chat

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
const UIBase = UIPrefix

// ListLimit caps how many messages one render of the tab pulls out of the
// store.
const ListLimit = 500

// LongTextThreshold is how many runes a message shows before the rest collapses
// behind a toggle. Past this a single post starts dominating the stream instead
// of reading like one message in it.
const LongTextThreshold = 600

// uiHandler serves the chat tab: a channel sidebar on the left, the selected
// channel's message stream on the right, replies nested under their parent.
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
	mux.HandleFunc("GET /channels/{key}", h.channel)
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

	Search   string
	Channels []channelView
	Selected *streamView
	Total    int

	// Info describes this plugin's providers, scoped to the chat tab, feeding
	// the shared how-to-test panel with snippets already rendered against the
	// ports this instance actually bound.
	Info []plugin.PluginInfo
}

// HowToTest carries the panel that says how to get messages in here, open when
// nothing has arrived yet - which is exactly when someone needs it.
func (v tabView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: !v.HasMessages()}
}

// HasMessages reports whether anything has been captured at all.
func (v tabView) HasMessages() bool { return v.Total > 0 }

// ListURL is where the channel sidebar is refetched from.
func (v tabView) ListURL() string {
	q := url.Values{}
	if v.Search != "" {
		q.Set("search", v.Search)
	}
	if v.Selected != nil {
		q.Set("channel", v.Selected.Key)
	}
	if len(q) == 0 {
		return v.Base + "/list"
	}
	return v.Base + "/list?" + q.Encode()
}

// EmptyState is what an empty tab shows: the providers that can fill it.
func (v tabView) EmptyState() components.EmptyState {
	var providers []plugin.ProviderInfo
	for _, p := range v.Info {
		providers = append(providers, p.Providers...)
	}
	return components.EmptyState{
		Title:     "No chat messages yet",
		Message:   "Point your application's Slack or Teams webhook at tommy and every message it posts lands here, grouped by channel with its replies nested underneath.",
		Providers: providers,
	}
}

// channelView is one entry of the sidebar.
type channelView struct {
	Key      string
	URL      string
	Title    string
	ID       string
	Author   string
	Preview  string
	At       time.Time
	Count    int
	Threads  int
	Orphans  int
	Selected bool
	Badges   []components.Badge
}

// streamView is the right-hand pane: one channel's messages.
type streamView struct {
	Key     string
	URL     string
	Title   string
	ID      string
	At      time.Time
	Count   int
	Threads []threadView
}

// threadView is a root message and the replies nested under it.
type threadView struct {
	Key    string
	RootID string
	// Orphan says the parent was never captured or has been evicted from the
	// ring buffer. The replies are still shown; the thread just says so.
	Orphan  bool
	Root    *messageView
	Replies []messageView
}

// ReplyCount is how many replies hang under the root.
func (t threadView) ReplyCount() int { return len(t.Replies) }

// messageView is one message in the stream. Author, Text and everything derived
// from them are untrusted input: they are interpolated as plain strings through
// html/template and never as template.HTML.
type messageView struct {
	ID       string
	EventURL string
	At       time.Time
	Author   string
	Initials string
	IconURL  string
	Bot      bool
	Reply    bool

	Text string
	// TextLong, TextPreview and TextLen let the template collapse a very long
	// message behind a toggle. TextPreview is truncated but, like Text, still
	// plain untrusted text.
	TextLong    bool
	TextPreview string
	TextLen     int

	Badges   []components.Badge
	Contents []contentView
}

// contentView is one piece of structured content as the tab shows it.
//
// Rich is the card-rendering seam: when a RichRenderer handles the format, its
// HTML is used; otherwise the message's plain text plus Inspector - the shared
// collapsible JSON inspector over the verbatim payload - is what the reader
// gets. That fallback is deliberately shipped first so capture never waits on
// rendering fidelity.
type contentView struct {
	Format    string
	Label     string
	Known     bool
	Rich      template.HTML
	HasRich   bool
	Inspector components.JSONView
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("channel"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, v)
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("channel"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "chat-channels", v)
}

func (h *uiHandler) channel(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.PathValue("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v.Selected == nil || v.Selected.Key != r.PathValue("key") {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if !coreui.IsPartial(r) {
		// A deep link renders the whole tab with the channel open, so a channel
		// URL can be pasted into a bug report.
		h.renderPage(w, r, v)
		return
	}
	h.writeFragment(w, "chat-stream", v)
}

func (h *uiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Store.Clear(r.Context(), PluginName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, err := h.view(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The whole tab comes back, because clearing empties the stream pane too.
	h.writeFragment(w, "chat-tab", v)
}

func (h *uiHandler) view(r *http.Request, selectedKey string) (tabView, error) {
	shell := coreui.ShellFrom(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))

	events, err := h.deps.Store.List(r.Context(), store.Query{
		Plugin: PluginName,
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
		StreamEvent: TypeMessage,
		Search:      search,
		Info:        providerInfo(shell),
	}

	channels := Channels(events)
	for _, c := range channels {
		v.Total += c.Count()
	}

	selected := FindChannel(channels, selectedKey)
	if selected == nil && selectedKey == "" && len(channels) > 0 {
		// A chat client opens on a channel rather than on a blank pane, and the
		// most recently active one is the one somebody just sent something to.
		selected = channels[0]
	}
	for _, c := range channels {
		v.Channels = append(v.Channels, h.channelView(c, c == selected))
	}
	if selected != nil {
		stream := h.streamView(selected)
		v.Selected = &stream
	}
	return v, nil
}

func (h *uiHandler) channelView(c *Channel, selected bool) channelView {
	cv := channelView{
		Key:      c.Key,
		URL:      UIBase + "/channels/" + c.Key,
		Title:    c.Display(),
		ID:       c.ID,
		At:       c.Latest,
		Count:    c.Count(),
		Threads:  c.ThreadCount(),
		Orphans:  c.Orphans(),
		Selected: selected,
	}
	if last, ok := c.Last(); ok {
		cv.Author = last.Message.Author.Display()
		cv.Preview = components.Truncate(90, last.Message.Preview())
		if b, ok := providerBadge(last.Event.Provider); ok {
			cv.Badges = append(cv.Badges, b)
		}
	}
	if n := c.ReplyCount(); n > 0 {
		cv.Badges = append(cv.Badges, components.Badge{
			Label: strconv.Itoa(n) + " " + plural(n, "reply", "replies"),
			Tone:  "info",
			Title: "Replies nested under a parent message in this channel",
		})
	}
	if cv.Orphans > 0 {
		cv.Badges = append(cv.Badges, orphanBadge(cv.Orphans))
	}
	return cv
}

func (h *uiHandler) streamView(c *Channel) streamView {
	s := streamView{
		Key:   c.Key,
		URL:   UIBase + "/channels/" + c.Key,
		Title: c.Display(),
		ID:    c.ID,
		At:    c.Latest,
		Count: c.Count(),
	}
	for _, t := range c.Threads {
		tv := threadView{Key: t.Key, RootID: t.RootID, Orphan: t.Orphan()}
		if t.Root != nil {
			root := h.messageView(*t.Root)
			tv.Root = &root
		}
		for _, reply := range t.Replies {
			tv.Replies = append(tv.Replies, h.messageView(reply))
		}
		s.Threads = append(s.Threads, tv)
	}
	return s
}

func (h *uiHandler) messageView(c Captured) messageView {
	m := c.Message
	mv := messageView{
		ID:       string(c.Event.ID),
		EventURL: coreui.EventURL("", c.Event.ID),
		At:       c.Event.ReceivedAt,
		Author:   m.Author.Display(),
		Initials: m.Author.Initials(),
		IconURL:  m.Author.IconURL,
		Bot:      m.Author.Bot,
		Reply:    m.IsReply(),
		Text:     m.Text,
		Badges:   messageBadges(m, c.Event.Provider),
	}
	if n := len([]rune(mv.Text)); n > LongTextThreshold {
		mv.TextLong = true
		mv.TextLen = n
		mv.TextPreview = components.Truncate(LongTextThreshold, mv.Text)
	}
	for _, content := range m.Contents {
		mv.Contents = append(mv.Contents, h.contentView(content))
	}
	return mv
}

func (h *uiHandler) contentView(c Content) contentView {
	cv := contentView{
		Format:    string(c.Format),
		Label:     c.Format.Label(),
		Known:     c.Format.Known(),
		Inspector: components.NewJSONView(c.Format.Label(), c.Value()),
	}
	if h.plugin != nil && h.plugin.rich != nil {
		if html, ok := h.plugin.rich(string(c.Format), c.Data); ok {
			cv.Rich, cv.HasRich = html, true
		}
	}
	return cv
}

// messageBadges is the row under a message: who captured it, whether it is a
// reply, and which structured payloads it carries.
func messageBadges(m *Message, provider string) []components.Badge {
	var badges []components.Badge
	if m.Author.Bot {
		badges = append(badges, components.Badge{
			Label: "bot",
			Tone:  "info",
			Title: "Posted by a bot or an incoming webhook rather than a person",
		})
	}
	if m.IsReply() {
		badges = append(badges, components.Badge{
			Label: "reply",
			Tone:  "muted",
			Title: "Replies to " + m.ThreadTS,
		})
	}
	for _, content := range m.Contents {
		tone := "warn"
		title := "Structured content in a schema tommy has no name for; shown as JSON"
		if content.Format.Known() {
			tone = "muted"
			title = "Structured content, kept verbatim as " + string(content.Format)
		}
		badges = append(badges, components.Badge{Label: content.Format.Label(), Tone: tone, Title: title})
	}
	if b, ok := providerBadge(provider); ok {
		badges = append(badges, b)
	}
	return badges
}

// providerBadge labels which provider captured a message. It is deliberately
// muted rather than colored like a status: it is provenance, not a state.
func providerBadge(provider string) (components.Badge, bool) {
	if provider == "" {
		return components.Badge{}, false
	}
	return components.Badge{
		Label: provider,
		Tone:  "muted",
		Title: "Captured by the " + provider + " provider",
	}, true
}

func orphanBadge(n int) components.Badge {
	return components.Badge{
		Label: strconv.Itoa(n) + " orphaned",
		Tone:  "warn",
		Title: "Threads whose parent message tommy never saw - it was posted before tommy started, or it has been evicted from the ring buffer",
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// providerInfo pulls this plugin's own entry out of the shell, so the
// how-to-test panel and the empty state show snippets rendered against the
// ports actually in use. It falls back to the plugin's own list, without
// snippets, when there is no registry - which happens only in a handler tested
// in isolation.
func providerInfo(shell *coreui.Shell) []plugin.PluginInfo {
	for _, p := range shell.Info() {
		if p.Name == PluginName {
			return []plugin.PluginInfo{p}
		}
	}
	return nil
}

func (h *uiHandler) renderPage(w http.ResponseWriter, r *http.Request, v tabView) {
	body, err := h.render("chat-tab", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "Chat", body); err != nil {
		h.deps.Logger.Warn("render chat tab", "err", err)
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
	// Safe: the buffer is html/template output, so every untrusted value in it
	// was already escaped in its own context.
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
