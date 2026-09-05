package push

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	coreui "github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// UIBase is where the tab is mounted once the core has taken its prefix off.
const UIBase = UIPrefix

// ListLimit caps how many pushes one render of the tab pulls out of the store.
const ListLimit = 500

// uiHandler serves the push tab: a phone lock screen on the left carrying one
// card per captured push, the selected push broken down on the right.
//
// The lock screen is the whole reason this tab exists rather than the generic
// event view. A JSON dump of an aps dictionary does not tell you that the push
// you are staring at displays nothing at all; a card that is visibly not a
// notification does.
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
	mux.HandleFunc("GET /messages/{id}", h.detail)
	mux.HandleFunc("DELETE /events", h.clear)
	// GET /events/{id} is deliberately left to the core's generic view, so an
	// event id from the API or the overview still opens a raw inspector here.
}

// tabView is everything the tab template renders.
type tabView struct {
	Base        string
	UIBase      string
	APIBase     string
	StreamEvent string

	Search   string
	Messages []listRow
	Selected *detailView
	Total    int
	// Silent counts the captured pushes that displayed nothing, which is the
	// number somebody scanning the tab is looking for.
	Silent int

	// Info feeds the shared how-to-test panel, carrying snippets already
	// rendered against the ports this instance actually bound.
	Info []plugin.PluginInfo
}

// HowToTest carries the panel that says how to get pushes in here, open when
// nothing has arrived yet - which is exactly when someone needs it.
func (v tabView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: !v.HasMessages()}
}

// HasMessages reports whether anything has been captured at all.
func (v tabView) HasMessages() bool { return v.Total > 0 }

// ListURL is where the lock screen is refetched from.
func (v tabView) ListURL() string {
	if v.Search == "" {
		return v.Base + "/list"
	}
	return v.Base + "/list?search=" + url.QueryEscape(v.Search)
}

// card is one lock-screen notification. Every string on it is captured text
// and is interpolated by the template as a plain string.
type card struct {
	// App is the line a phone puts above the notification: the bundle ID or
	// the Firebase project, since tommy has no app name to show.
	App string
	// Kind drives the styling, so a silent push cannot be mistaken for one
	// that displays.
	Kind Kind
	// Displays is Kind.Displays, hoisted so the template does not call a
	// method on a bare string.
	Displays bool
	// Explain is the sentence shown on a card that displays nothing.
	Explain string

	Title    string
	Subtitle string
	Body     string

	// Note carries what a card with no text has instead: "badge 3", "sound
	// chime.aiff", or the data keys of a silent push.
	Note string
	// Image is a URL the sender asked the device to fetch. It is rendered as
	// text, never as an <img> or an href: it points at somebody else's server
	// and nothing on this page should make a browser go there.
	Image string
	// LocalizedNote names the resource keys when the text is a key rather than
	// the text, which is why the card looks empty on a real device too.
	LocalizedNote string
}

// listRow is one push on the lock screen.
type listRow struct {
	ID       string
	URL      string
	At       time.Time
	Selected bool
	Card     card
	Target   string
	Badges   []components.Badge
}

// detailView is the right-hand pane.
type detailView struct {
	ID       string
	Title    string
	At       time.Time
	Provider string
	RawURL   string
	EventURL string
	Badges   []components.Badge

	Kind     Kind
	Displays bool
	Explain  string
	Card     card

	Routing  components.KVTable
	Delivery components.KVTable
	Alert    components.KVTable

	Data     *components.JSONView
	DataKeys []string
	Payloads []components.JSONView

	Raw event.Raw
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, v)
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "push-list", v)
}

func (h *uiHandler) detail(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v.Selected == nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if !coreui.IsPartial(r) {
		// A deep link renders the whole tab with the push open, so a message
		// URL can be pasted into a bug report.
		h.renderPage(w, r, v)
		return
	}
	h.writeFragment(w, "push-detail", v)
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
	// The whole tab comes back, because clearing empties the detail pane too.
	h.writeFragment(w, "push-tab", v)
}

func (h *uiHandler) view(r *http.Request, selectedID string) (tabView, error) {
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
		UIBase:      coreui.ShellFrom(r).UIBase,
		APIBase:     APIBase,
		StreamEvent: EventType,
		Search:      search,
		Info:        coreui.ShellFrom(r).Info(),
	}

	captured := Messages(events)
	v.Total = len(captured)
	// Nothing selected means the newest push, so the tab is never a blank pane
	// next to a full lock screen.
	if selectedID == "" && len(captured) > 0 {
		selectedID = string(captured[0].Event.ID)
	}
	for _, c := range captured {
		if !c.Message.Displays() {
			v.Silent++
		}
		row := listRow{
			ID:       string(c.Event.ID),
			URL:      UIBase + "/messages/" + string(c.Event.ID),
			At:       c.Event.ReceivedAt,
			Selected: string(c.Event.ID) == selectedID,
			Card:     newCard(c.Message),
			Target:   c.Message.Target.Display(),
			Badges:   messageBadges(c.Message, c.Event.Provider),
		}
		v.Messages = append(v.Messages, row)
		if row.Selected {
			d := h.detailView(c)
			v.Selected = &d
		}
	}
	return v, nil
}

// newCard builds the lock-screen card. It decides here, not in the template,
// what a card with no text says instead, so the decision can be tested.
func newCard(m *Message) card {
	c := card{
		App:      m.App,
		Kind:     m.Kind,
		Displays: m.Displays(),
		Explain:  m.Kind.Explain(),
	}
	if c.App == "" {
		c.App = "(no app id)"
	}
	if a := m.Alert; a != nil {
		c.Title = a.Title
		c.Subtitle = a.Subtitle
		c.Body = a.Body
		c.Image = a.Image
		if l := a.Localization; l != nil {
			var parts []string
			if l.TitleKey != "" {
				parts = append(parts, "title "+l.TitleKey)
			}
			if l.BodyKey != "" {
				parts = append(parts, "body "+l.BodyKey)
			}
			if len(parts) > 0 {
				c.LocalizedNote = "localized: " + strings.Join(parts, ", ") +
					" — the device shows whatever these keys resolve to in the app bundle"
			}
		}
		if !a.HasBanner() {
			c.Note = badgeAndSound(a)
		}
	}
	if c.Note == "" && !c.Displays {
		if keys := m.DataKeys(); len(keys) > 0 {
			c.Note = "data: " + strings.Join(keys, ", ")
		}
	}
	return c
}

func (h *uiHandler) detailView(c Captured) detailView {
	m := c.Message
	d := detailView{
		ID:       string(c.Event.ID),
		Title:    m.Title(),
		At:       c.Event.ReceivedAt,
		Provider: c.Event.Provider,
		RawURL:   APIBase + "/messages/" + string(c.Event.ID) + "/raw",
		EventURL: coreui.EventURL("", c.Event.ID),
		Badges:   messageBadges(m, c.Event.Provider),
		Kind:     m.Kind,
		Displays: m.Displays(),
		Explain:  m.Kind.Explain(),
		Card:     newCard(m),
		Routing:  routingTable(m),
		Delivery: deliveryTable(m, c.Event.ReceivedAt),
		Alert:    alertTable(m),
		DataKeys: m.DataKeys(),
		Raw:      c.Event.Raw,
	}
	if m.HasData() {
		view := components.NewJSONView("Data for the app", m.DataValue())
		d.Data = &view
	}
	for _, p := range m.Payloads {
		d.Payloads = append(d.Payloads, components.NewJSONView(p.Format.Label(), p.Value()))
	}
	return d
}

// routingTable says where the push went and what it was for. It spells the
// target kind out rather than showing a bare string, because "device" and
// "topic" are the difference between one delivery and an unknown number.
func routingTable(m *Message) components.KVTable {
	t := components.KVTable{Caption: "Where it went"}
	add := func(k, v string) {
		if v != "" {
			t.Rows = append(t.Rows, components.KV{Key: k, Value: v})
		}
	}
	if m.Target.Empty() {
		add("Target", "(none — the request named no device, topic or condition)")
	} else {
		add(capitalize(m.Target.Kind.Label()), m.Target.Value)
	}
	if m.Target.Source != "" {
		add("Read from", targetSourceLabel(m.Target))
	}
	if m.Target.Kind.Fanout() {
		add("Fan-out", "one message, delivered to every subscriber of this "+m.Target.Kind.Label())
	}
	add("App", m.App)
	add("Declared push type", m.PushType)
	return t
}

// targetSourceLabel explains where in the request the address was found. It is
// worth a whole row: APNs puts the device token in the URL and FCM puts it in
// the body, and a provider author reading a capture should see which.
func targetSourceLabel(t Target) string {
	switch t.Source {
	case "path":
		return "the request path (POST /3/device/{token})"
	case "token":
		return `the body field "token"`
	case "fid":
		return `the body field "fid" (a Firebase Installation ID)`
	case "topic", "condition":
		return `the body field "` + t.Source + `"`
	default:
		return t.Source
	}
}

func deliveryTable(m *Message, receivedAt time.Time) components.KVTable {
	t := components.KVTable{Caption: "How it was to be delivered"}
	add := func(k, v string) {
		if v != "" {
			t.Rows = append(t.Rows, components.KV{Key: k, Value: v})
		}
	}
	d := m.Delivery
	switch {
	case d.Priority != "" && d.PriorityRaw != "" && d.PriorityRaw != string(d.Priority):
		add("Priority", string(d.Priority)+" (sent as "+d.PriorityRaw+")")
	case d.Priority != "":
		add("Priority", string(d.Priority))
	case d.PriorityRaw != "":
		add("Priority", d.PriorityRaw+" (not a value this plugin recognizes)")
	}
	if e := d.Expiry; e != nil {
		v := e.Describe()
		if deadline, ok := e.Deadline(receivedAt); ok && e.TTLSeconds != nil {
			v += " — by " + deadline.Format(time.RFC3339) + " counting from capture"
		}
		if e.Raw != "" && !strings.Contains(v, e.Raw) {
			v += " (sent as " + e.Raw + ")"
		}
		add("Expiry", v)
	}
	add("Collapse key", d.CollapseKey)
	if len(t.Rows) == 0 {
		add("Delivery", "nothing specified; the platform's defaults apply")
	}
	return t
}

func alertTable(m *Message) components.KVTable {
	t := components.KVTable{Caption: "What it displays"}
	add := func(k, v string) {
		if v != "" {
			t.Rows = append(t.Rows, components.KV{Key: k, Value: v})
		}
	}
	a := m.Alert
	if a == nil {
		add("Alert", "none — this push carries no display payload at all")
		return t
	}
	add("Title", a.Title)
	add("Subtitle", a.Subtitle)
	add("Body", a.Body)
	if a.Badge != nil {
		add("Badge", badgeText(*a.Badge))
	}
	add("Sound", a.Sound)
	add("Category", a.Category)
	add("Image URL", a.Image)
	if l := a.Localization; l != nil {
		add("Title key", l.TitleKey)
		if len(l.TitleArgs) > 0 {
			add("Title args", strings.Join(l.TitleArgs, ", "))
		}
		add("Body key", l.BodyKey)
		if len(l.BodyArgs) > 0 {
			add("Body args", strings.Join(l.BodyArgs, ", "))
		}
	}
	return t
}

// messageBadges is the badge row on a card and above the breakdown. The kind
// badge is first and always present: it is the fact the tab exists to show.
func messageBadges(m *Message, provider string) []components.Badge {
	badges := []components.Badge{{
		Label: m.Kind.Label(),
		Tone:  kindTone(m.Kind),
		Title: m.Kind.Explain(),
	}}
	if m.PushType != "" {
		badges = append(badges, components.Badge{
			Label: m.PushType,
			Tone:  "info",
			Title: "What the sender declared this to be (apns-push-type)",
		})
	}
	if m.Target.Kind.Fanout() {
		badges = append(badges, components.Badge{
			Label: m.Target.Kind.Label(),
			Tone:  "warn",
			Title: "Addressed to a " + m.Target.Kind.Label() + ", so it fans out to every subscriber",
		})
	}
	if p := m.Delivery.Priority; p != "" {
		badges = append(badges, components.Badge{
			Label: "priority " + string(p),
			Tone:  "muted",
			Title: "Sent as " + m.Delivery.PriorityRaw,
		})
	}
	if k := m.Delivery.CollapseKey; k != "" {
		badges = append(badges, components.Badge{
			Label: "collapse " + k,
			Tone:  "muted",
			Title: "Supersedes any undelivered message with the same collapse key",
		})
	}
	if n := len(m.DataKeys()); n > 0 {
		badges = append(badges, components.Badge{
			Label: strconv.Itoa(n) + " data key" + plural(n),
			Tone:  "muted",
			Title: strings.Join(m.DataKeys(), ", "),
		})
	}
	if provider != "" {
		badges = append(badges, components.Badge{
			Label: provider,
			Tone:  "muted",
			Title: "Captured by the " + provider + " provider",
		})
	}
	return badges
}

func kindTone(k Kind) string {
	switch k {
	case KindNotification:
		return "ok"
	case KindSilent:
		return "warn"
	case KindEmpty:
		return "error"
	default:
		return "muted"
	}
}

func (h *uiHandler) renderPage(w http.ResponseWriter, r *http.Request, v tabView) {
	body, err := h.render("push-tab", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "Push", body); err != nil {
		h.deps.Logger.Warn("render push tab", "err", err)
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

// capitalize upper-cases the first letter of a label. The target kinds are
// lowercase constants and a table key reads better capitalized.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
