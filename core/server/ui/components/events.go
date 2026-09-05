package components

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

// EventFilter is the state of the generic event view's filter bar.
type EventFilter struct {
	Search   string
	Type     string
	Provider string
}

// EventView is the data behind the generic event view - a filterable event
// table plus a raw/JSON/hex inspector. Any plugin that does not mount its own
// tab gets this for free.
type EventView struct {
	// Base is the URL prefix the view is mounted at, "/ui/mail" or "/ui".
	Base string
	// Plugin is the plugin being shown, "" for the cross-plugin overview.
	Plugin string
	Title  string

	Filter   EventFilter
	Events   []*event.Event
	Selected *event.Event

	// Info describes the plugins in scope: one for a plugin tab, all of them
	// for the cross-plugin overview. It feeds the "How to test" panel and the
	// empty state.
	Info []plugin.PluginInfo

	// Types and ProviderNames populate the filter dropdowns.
	Types         []string
	ProviderNames []string

	// PageBase is where an event's own page lives, "/ui/events/". The view
	// links to it rather than building the path itself, so the components
	// package stays ignorant of where the UI is mounted.
	PageBase string
}

// PageURL is the standalone page of one event: the URL to paste into a bug
// report or open from a log line. Empty when the view was built without a
// PageBase, which is how a plugin rendering this view outside the server gets
// no dangling link.
func (v EventView) PageURL(id event.ID) string {
	if v.PageBase == "" {
		return ""
	}
	return v.PageBase + string(id)
}

// HowToTest describes the plugins in scope, opening the panel when there is
// nothing else on screen to look at.
func (v EventView) HowToTest() HowToTest {
	return HowToTest{Info: v.Info, Open: len(v.Events) == 0}
}

// Providers flattens Info into the provider cards the empty state shows.
func (v EventView) Providers() []plugin.ProviderInfo {
	var out []plugin.ProviderInfo
	for _, p := range v.Info {
		out = append(out, p.Providers...)
	}
	return out
}

// EmptyState is what the detail pane and an empty list show: the snippets that
// put something into this tab.
func (v EventView) EmptyState() EmptyState {
	title := "Nothing captured yet"
	msg := "Send something to tommy and it will show up here, live."
	if v.Selected == nil && len(v.Events) > 0 {
		title = "Pick an event"
		msg = "Select an event on the left to inspect its payload, metadata and raw request."
		return EmptyState{Title: title, Message: msg}
	}
	return EmptyState{Title: title, Message: msg, Providers: v.Providers()}
}

// DetailURL is where the detail pane of an event is fetched from.
func (v EventView) DetailURL(id event.ID) string {
	return v.Base + "/events/" + string(id)
}

// ListURL is where the list fragment is refetched from.
func (v EventView) ListURL() string { return v.Base + "/list" }

// IsSelected reports whether e is the event shown in the detail pane.
func (v EventView) IsSelected(e *event.Event) bool {
	return v.Selected != nil && v.Selected.ID == e.ID
}

// EventTable turns the view's events into a Table, so the generic view is built
// from the same primitives a bespoke plugin tab would use.
func (v EventView) EventTable() Table {
	t := Table{
		Columns: []string{"Time", "Source", "Title", ""},
		Empty:   "No events captured yet.",
	}
	for _, e := range v.Events {
		badges := template.HTML(fmt.Sprintf(
			`<span class="badge badge-info">%s</span> <span class="badge badge-muted">%s</span>`,
			template.HTMLEscapeString(e.Provider), template.HTMLEscapeString(e.Type)))
		title := e.Summary.Title
		if title == "" {
			title = "(no title)"
		}
		t.Rows = append(t.Rows, Row{
			Href:     v.DetailURL(e.ID),
			Target:   "#detail",
			Selected: v.IsSelected(e),
			Cells: []Cell{
				{Text: e.ReceivedAt.Local().Format("15:04:05"), Class: "mono nowrap"},
				{HTML: badges, Class: "nowrap"},
				{HTML: template.HTML(
					`<strong>` + template.HTMLEscapeString(Truncate(70, title)) + `</strong><br><span class="muted">` +
						template.HTMLEscapeString(Truncate(90, e.Summary.Snippet)) + `</span>`)},
				{Text: fmt.Sprintf("%d B", len(e.Raw.Body)), Class: "mono muted nowrap"},
			},
		})
	}
	return t
}

// SummaryTable is the key/value header block of the detail pane.
func SummaryTable(e *event.Event) KVTable {
	t := KVTable{Caption: "Event"}
	t.Rows = append(t.Rows,
		KV{Key: "ID", Value: string(e.ID)},
		KV{Key: "Received", Value: e.ReceivedAt.Local().Format("2006-01-02 15:04:05.000 MST")},
		KV{Key: "Plugin", Value: e.Plugin},
		KV{Key: "Provider", Value: e.Provider},
		KV{Key: "Type", Value: e.Type},
	)
	if e.Summary.From != "" {
		t.Rows = append(t.Rows, KV{Key: "From", Value: e.Summary.From})
	}
	if len(e.Summary.To) > 0 {
		t.Rows = append(t.Rows, KV{Key: "To", Value: joinComma(e.Summary.To)})
	}
	if e.Summary.Title != "" {
		t.Rows = append(t.Rows, KV{Key: "Title", Value: e.Summary.Title})
	}
	return t
}

// EventPageLink is one step of the newer/older navigation on an event page.
type EventPageLink struct {
	Href  string
	Title string
}

// EventPage is the standalone page for a single event - the one URL a captured
// event can be linked by, and what an application's own logs point at.
//
// Body is the rendered detail: the owning plugin's own view when it has one,
// the generic inspector otherwise. It arrives as already-rendered HTML because
// a plugin renders it from its own template set.
type EventPage struct {
	Event *event.Event
	Body  template.HTML

	// PluginTitle and PluginHref point back at the tab this event belongs to.
	// Href is empty when the plugin is not mounted, which is what a link to an
	// event captured by a since-disabled plugin looks like.
	PluginTitle string
	PluginHref  string

	// ShareURL is absolute, because the point of the copy button is to paste
	// the link somewhere that is not this browser tab.
	ShareURL string
	// APIHref is the same event as JSON.
	APIHref string

	// Newer and Older walk the events of the same plugin, so an inbox can be
	// read from the page rather than the list.
	Newer *EventPageLink
	Older *EventPageLink
}

// Heading is what the page calls the event.
func (p EventPage) Heading() string {
	if p.Event == nil {
		return "Event"
	}
	if p.Event.Summary.Title != "" {
		return p.Event.Summary.Title
	}
	return "(no title)"
}

// RawMeta renders the transport metadata of a Raw as a key/value table.
func RawMeta(raw event.Raw) KVTable {
	t := KVTable{}
	if raw.Transport != "" {
		t.Rows = append(t.Rows, KV{Key: "Transport", Value: raw.Transport})
	}
	if raw.PeerAddr != "" {
		t.Rows = append(t.Rows, KV{Key: "Peer", Value: raw.PeerAddr})
	}
	if raw.Method != "" || raw.Path != "" {
		t.Rows = append(t.Rows, KV{Key: "Request", Value: raw.Method + " " + raw.Path})
	}
	t.Rows = append(t.Rows, KV{Key: "Size", Value: BytesHuman(int64(len(raw.Body)))})
	if len(raw.Headers) > 0 {
		t.Rows = append(t.Rows, KV{Key: "Headers", HTML: headersHTML(raw.Headers)})
	}
	return t
}

func headersHTML(h http.Header) template.HTML {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := `<dl class="headers">`
	for _, k := range keys {
		for _, v := range h[k] {
			out += `<dt>` + template.HTMLEscapeString(k) + `</dt><dd>` + template.HTMLEscapeString(v) + `</dd>`
		}
	}
	return template.HTML(out + `</dl>`)
}

func joinComma(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// HowToTest is the panel that says how to put something into a view.
type HowToTest struct {
	Info []plugin.PluginInfo
	Open bool
}

// EmptyState is the "nothing here yet, here is how to put something in it"
// panel.
type EmptyState struct {
	Title     string
	Message   string
	Providers []plugin.ProviderInfo
}
