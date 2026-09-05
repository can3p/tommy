package hl7

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

// ListLimit caps how many messages one render of the tab pulls out of the
// store.
const ListLimit = 500

// OpenSegments is how many segments of a message start expanded. An ADT is a
// handful of segments and wants to be read whole; an ORU with fifty OBX
// segments wants to be scanned first, so the tail starts collapsed.
const OpenSegments = 15

// EmptyFieldsShown caps the "empty fields" footer of a segment, so a sparse
// segment with sixty unused positions does not bury the two that matter.
const EmptyFieldsShown = 24

// uiHandler serves the HL7 tab: a message list on the left, the selected
// message as a segment tree on the right.
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

	// Info feeds the shared how-to-test panel, carrying snippets already
	// rendered against the ports this instance actually bound.
	Info []plugin.PluginInfo
}

// HowToTest carries the panel that says how to get messages in here, open when
// nothing has arrived yet - which is exactly when someone needs it.
func (v tabView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: !v.HasMessages()}
}

// HasMessages reports whether anything has been captured at all.
func (v tabView) HasMessages() bool { return v.Total > 0 }

// ListURL is where the message list fragment is refetched from.
func (v tabView) ListURL() string {
	if v.Search == "" {
		return v.Base + "/list"
	}
	return v.Base + "/list?search=" + url.QueryEscape(v.Search)
}

// listRow is one message in the left-hand list. Every string on it is captured
// text and is interpolated by the template as a plain string.
type listRow struct {
	ID          string
	URL         string
	At          time.Time
	MessageType string
	ControlID   string
	Sender      string
	Receiver    string
	Preview     string
	Selected    bool
	Badges      []components.Badge
}

// detailView is the right-hand pane: the message as a segment tree, its header
// summarized, and the bytes it arrived as.
type detailView struct {
	ID       string
	Title    string
	At       time.Time
	Provider string
	RawURL   string
	EventURL string
	Badges   []components.Badge
	Header   components.KVTable
	Segments []segmentView
	Issues   []Issue
	Raw      event.Raw
}

// segmentView is one segment of the tree.
type segmentView struct {
	Label string
	Name  string
	// Open decides whether the segment starts expanded.
	Open bool
	Rows []valueRow
	// Empty names the field positions that arrived empty, listed once at the
	// end rather than as a row each: a segment is mostly empty positions, and
	// a row per position would drown the values that are actually there.
	Empty []string
	// MoreEmpty counts the empty positions past EmptyFieldsShown.
	MoreEmpty int
}

// valueRow is one line of a segment: a path, an optional dictionary name and
// the value. Depth drives the indent; Kind drives the styling.
type valueRow struct {
	Path  string
	Name  string
	Value string
	Depth int
	// Kind is "field", "repetition", "component" or "subcomponent".
	Kind string
	// Note carries a short remark shown instead of a value, such as how many
	// times a field repeats.
	Note string
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
	h.writeFragment(w, "hl7-list", v)
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
		// A deep link renders the whole tab with the message open, so a
		// message URL can be pasted into a bug report.
		h.renderPage(w, r, v)
		return
	}
	h.writeFragment(w, "hl7-detail", v)
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
	h.writeFragment(w, "hl7-tab", v)
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
	// Nothing selected means the newest message, so the tab is never a blank
	// pane next to a full list.
	if selectedID == "" && len(captured) > 0 {
		selectedID = string(captured[0].Event.ID)
	}
	for _, c := range captured {
		row := h.listRow(c, string(c.Event.ID) == selectedID)
		v.Messages = append(v.Messages, row)
		if row.Selected {
			d := h.detailView(c)
			v.Selected = &d
		}
	}
	return v, nil
}

func (h *uiHandler) listRow(c Captured, selected bool) listRow {
	m := c.Message
	return listRow{
		ID:          string(c.Event.ID),
		URL:         UIBase + "/messages/" + string(c.Event.ID),
		At:          c.Event.ReceivedAt,
		MessageType: fallback(m.Header.MessageType(), "(no message type)"),
		ControlID:   m.Header.ControlID,
		Sender:      m.Header.Sender(),
		Receiver:    m.Header.Receiver(),
		Preview:     m.Preview(),
		Selected:    selected,
		Badges:      messageBadges(m, c.Event.Provider),
	}
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
		Header:   headerTable(m),
		Issues:   m.Issues,
		Raw:      c.Event.Raw,
	}
	for i, seg := range m.Segments {
		d.Segments = append(d.Segments, newSegmentView(seg, i < OpenSegments))
	}
	return d
}

// headerTable summarizes MSH for the top of the detail pane. Every value is
// captured text and reaches the page as a plain string.
func headerTable(m *Message) components.KVTable {
	t := components.KVTable{Caption: "Message header"}
	add := func(k, v string) {
		if v != "" {
			t.Rows = append(t.Rows, components.KV{Key: k, Value: v})
		}
	}
	add("Message type", m.Header.MessageType())
	add("Message structure", m.Header.Structure)
	add("Control ID", m.Header.ControlID)
	add("Sending", m.Header.Sender())
	add("Receiving", m.Header.Receiver())
	add("Sent at (MSH-7)", m.Header.Timestamp)
	add("Processing ID", m.Header.ProcessingID)
	add("HL7 version", m.Header.Version)
	sep := m.Separators.Field + m.Separators.EncodingCharacters()
	if !m.Separators.Standard() {
		sep += "  (not the conventional |^~\\&)"
	}
	add("Separators", sep)
	add("Segments", strconv.Itoa(len(m.Segments)))
	return t
}

// messageBadges is the badge row on a list entry and above the tree. The
// non-standard separator badge earns its place: a message that declared its own
// delimiters is exactly the one another parser is likely to be getting wrong,
// and nothing else on the page would say so.
func messageBadges(m *Message, provider string) []components.Badge {
	var badges []components.Badge
	if t := m.Header.MessageType(); t != "" {
		badges = append(badges, components.Badge{
			Label: t,
			Tone:  "info",
			Title: "MSH-9",
		})
	}
	if v := m.Header.Version; v != "" {
		badges = append(badges, components.Badge{Label: "v" + v, Tone: "muted", Title: "MSH-12"})
	}
	badges = append(badges, components.Badge{
		Label: strconv.Itoa(len(m.Segments)) + " segments",
		Tone:  "muted",
		Title: m.Outline(),
	})
	if !m.Separators.Standard() {
		badges = append(badges, components.Badge{
			Label: "separators " + m.Separators.Field + m.Separators.EncodingCharacters(),
			Tone:  "warn",
			Title: "This message declared its own delimiters in MSH-1 and MSH-2 rather than the conventional |^~\\&",
		})
	}
	if n := len(m.Issues); n > 0 {
		badges = append(badges, components.Badge{
			Label: strconv.Itoa(n) + " issue" + plural(n),
			Tone:  "error",
			Title: issueSummary(m.Issues),
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

func issueSummary(issues []Issue) string {
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		parts = append(parts, i.Detail)
	}
	return strings.Join(parts, "; ")
}

// newSegmentView flattens one segment into the rows the template renders. The
// flattening happens here rather than in the template so that it can be tested
// directly and so that the template stays a description of the page rather than
// a description of HL7.
func newSegmentView(seg Segment, open bool) segmentView {
	v := segmentView{
		Label: seg.Label(),
		Name:  SegmentName(seg.ID),
		Open:  open,
	}
	for _, f := range seg.Fields {
		path := seg.ID + "-" + strconv.Itoa(f.Position)
		if f.Empty() {
			if len(v.Empty) < EmptyFieldsShown {
				v.Empty = append(v.Empty, path)
			} else {
				v.MoreEmpty++
			}
			continue
		}
		name := FieldName(seg.ID, f.Position)
		if !f.Repeats() {
			rep := f.Repetition(1)
			v.Rows = append(v.Rows, valueRow{Path: path, Name: name, Value: f.Value, Kind: "field"})
			v.Rows = append(v.Rows, componentRows(path, rep, 1)...)
			continue
		}
		v.Rows = append(v.Rows, valueRow{
			Path: path,
			Name: name,
			Kind: "field",
			Note: strconv.Itoa(len(f.Repetitions)) + " repetitions",
		})
		for i, rep := range f.Repetitions {
			repPath := path + "[" + strconv.Itoa(i+1) + "]"
			v.Rows = append(v.Rows, valueRow{Path: repPath, Value: rep.Value, Depth: 1, Kind: "repetition"})
			v.Rows = append(v.Rows, componentRows(repPath, rep, 2)...)
		}
	}
	return v
}

// componentRows breaks a repetition down when there is anything to break down.
// A single-component, single-subcomponent value is already fully shown by the
// row above it, so it produces nothing: repeating "SMITH" under "SMITH" would
// be noise, and the point of the tree is that structure is visible.
func componentRows(base string, rep Repetition, depth int) []valueRow {
	if !rep.HasComponents() {
		return subcomponentRows(base+".1", rep.Component(1), depth)
	}
	var rows []valueRow
	for i, c := range rep.Components {
		if c.Value == "" {
			continue
		}
		path := base + "." + strconv.Itoa(i+1)
		rows = append(rows, valueRow{Path: path, Value: c.Value, Depth: depth, Kind: "component"})
		rows = append(rows, subcomponentRows(path, c, depth+1)...)
	}
	return rows
}

func subcomponentRows(base string, c Component, depth int) []valueRow {
	if !c.HasSubcomponents() {
		return nil
	}
	var rows []valueRow
	for i, s := range c.Subcomponents {
		if s == "" {
			continue
		}
		rows = append(rows, valueRow{
			Path:  base + "." + strconv.Itoa(i+1),
			Value: s,
			Depth: depth,
			Kind:  "subcomponent",
		})
	}
	return rows
}

func (h *uiHandler) renderPage(w http.ResponseWriter, r *http.Request, v tabView) {
	body, err := h.render("hl7-tab", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "HL7", body); err != nil {
		h.deps.Logger.Warn("render hl7 tab", "err", err)
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

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
