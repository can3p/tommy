package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/core/store"
)

// APIBase is where the plugin's routes end up once the core has mounted them.
// The handlers see the prefix stripped, so they need it spelled out to build
// the links they hand back to clients.
const APIBase = APIPrefix

// MessageEnvelope is what /messages returns: the canonical message plus the
// event fields a client needs to correlate it with the generic event log, plus
// the derived channel and thread keys so a client never has to re-derive them.
type MessageEnvelope struct {
	ID         event.ID       `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Provider   string         `json:"provider"`
	Type       string         `json:"type"`
	Meta       map[string]any `json:"meta,omitempty"`
	// URL is this message's own page in the UI: the link to open, or to print
	// in a log line, once something has been sent.
	URL string `json:"url,omitempty"`

	// ChannelKey and ThreadKey are the derived identifiers: the same ones
	// /channels reports and the tab uses in its URLs.
	ChannelKey string `json:"channel_key"`
	ThreadKey  string `json:"thread_key"`
	// RootID is the identity of the thread this message belongs to, in the
	// provider's own terms.
	RootID string `json:"root_id"`
	// Reply says the message hangs under a parent.
	Reply bool `json:"reply"`
	// Formats lists the structured-content schemas the message carries, so a
	// client can tell what it is looking at without walking Contents.
	Formats []Format `json:"formats,omitempty"`

	Message *Message `json:"message"`
}

// NewMessageEnvelope builds the API view of a captured message.
func NewMessageEnvelope(c Captured) MessageEnvelope {
	channelKey := ChannelKey(c.Message.Channel.ID)
	rootID := c.RootKey()
	var formats []Format
	for _, content := range c.Message.Contents {
		formats = append(formats, content.Format)
	}
	return MessageEnvelope{
		ID:         c.Event.ID,
		ReceivedAt: c.Event.ReceivedAt,
		Provider:   c.Event.Provider,
		Type:       c.Event.Type,
		Meta:       c.Event.Meta,
		ChannelKey: channelKey,
		ThreadKey:  ThreadKey(channelKey, rootID),
		RootID:     rootID,
		Reply:      c.Message.IsReply(),
		Formats:    formats,
		Message:    c.Message,
	}
}

// ChannelSummary is one entry of the derived channel index. It is computed from
// the event list on every request - nothing about a channel is stored - which
// is why it always agrees with what the tab shows.
type ChannelSummary struct {
	Key     string `json:"key"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Display string `json:"display"`

	Messages int `json:"messages"`
	Threads  int `json:"threads"`
	Replies  int `json:"replies"`
	// Orphans counts threads whose parent message was never captured or has
	// been evicted from the ring buffer.
	Orphans int `json:"orphans"`

	LastMessageAt time.Time `json:"last_message_at"`
	LastMessageID event.ID  `json:"last_message_id,omitempty"`
	LastAuthor    string    `json:"last_author,omitempty"`
	LastPreview   string    `json:"last_preview,omitempty"`

	// MessagesURL fetches just this channel's messages; UIURL opens it in the
	// chat tab.
	MessagesURL string `json:"messages_url"`
	UIURL       string `json:"ui_url"`
}

// NewChannelSummary describes one derived channel.
func NewChannelSummary(c *Channel) ChannelSummary {
	s := ChannelSummary{
		Key:           c.Key,
		ID:            c.ID,
		Name:          c.Name,
		Display:       c.Display(),
		Messages:      c.Count(),
		Threads:       c.ThreadCount(),
		Replies:       c.ReplyCount(),
		Orphans:       c.Orphans(),
		LastMessageAt: c.Latest,
		MessagesURL:   APIBase + "/messages?channel=" + url.QueryEscape(c.ID),
		UIURL:         UIPrefix + "/channels/" + c.Key,
	}
	if last, ok := c.Last(); ok {
		s.LastMessageID = last.Event.ID
		s.LastAuthor = last.Message.Author.Display()
		s.LastPreview = last.Message.Preview()
	}
	return s
}

// apiHandler serves /api/v1/chat/.
type apiHandler struct {
	deps plugin.Deps
}

// APIEndpoints documents what RegisterAPI mounts, and is what this plugin's
// own OpenAPI description is generated from.
func (p *Plugin) APIEndpoints() []plugin.APIEndpoint {
	list := append(plugin.CommonListParams(),
		plugin.APIParam{Name: "channel", Description: "The provider's channel id, its display name, or the derived key - all three appear in responses."},
		plugin.APIParam{Name: "author", Description: "Substring of the author's name or id."},
		plugin.APIParam{Name: "thread", Description: "Only messages in this thread, by derived thread key."},
		plugin.APIParam{Name: "format", Description: "The payload format the provider sent, such as blocks or text."},
		plugin.APIParam{Name: "bot", Description: "Only messages posted by a bot, or only those that were not.", Type: "boolean"},
		plugin.APIParam{Name: "replies", Description: "Include or exclude thread replies.", Type: "boolean"},
	)
	return []plugin.APIEndpoint{
		{Method: "GET", Path: "/messages", Description: "Every captured chat message, newest first.",
			Query: list, Response: []MessageEnvelope{}},
		{Method: "GET", Path: "/messages/{id}", Description: "One message, with its derived channel, thread and root identifiers.",
			Response: MessageEnvelope{}},
		{Method: "GET", Path: "/channels", Description: "The derived channel index: message, thread, reply and orphan counts, and the last message. Computed on every request, so it always agrees with the tab.",
			Response: []ChannelSummary{}},
		{Method: "DELETE", Path: "/messages", Description: "Clear every captured chat message.",
			Status: http.StatusNoContent},
	}
}

func (h *apiHandler) mount(mux plugin.Mux) {
	mux.HandleFunc("GET /messages", h.list)
	mux.HandleFunc("GET /messages/{id}", h.get)
	mux.HandleFunc("GET /channels", h.channels)
	mux.HandleFunc("DELETE /messages", h.clear)
}

// filters are the chat-specific narrowings, applied after the store's own.
type filters struct {
	channel string
	author  string
	thread  string
	format  string
	bot     bool
	botSet  bool
	replies bool
	repSet  bool
}

func parseFilters(r *http.Request) filters {
	v := r.URL.Query()
	f := filters{
		channel: strings.TrimSpace(v.Get("channel")),
		author:  strings.TrimSpace(v.Get("author")),
		thread:  strings.TrimSpace(v.Get("thread")),
		format:  strings.TrimSpace(v.Get("format")),
	}
	if raw := v.Get("bot"); raw != "" {
		f.botSet = true
		f.bot = truthy(raw)
	}
	if raw := v.Get("replies"); raw != "" {
		f.repSet = true
		f.replies = truthy(raw)
	}
	return f
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// match narrows one captured message. The channel filter accepts the provider's
// channel id, its display name or the derived key, because all three are things
// a caller has seen in an API response.
func (f filters) match(c Captured) bool {
	m := c.Message
	if f.channel != "" {
		key := ChannelKey(m.Channel.ID)
		if !strings.EqualFold(f.channel, m.Channel.ID) &&
			!strings.EqualFold(f.channel, m.Channel.Name) &&
			f.channel != key {
			return false
		}
	}
	if f.author != "" && !strings.EqualFold(f.author, m.Author.Display()) && !strings.EqualFold(f.author, m.Author.ID) {
		return false
	}
	if f.thread != "" {
		rootID := c.RootKey()
		if f.thread != rootID && f.thread != ThreadKey(ChannelKey(m.Channel.ID), rootID) {
			return false
		}
	}
	if f.format != "" {
		found := false
		for _, content := range m.Contents {
			if strings.EqualFold(f.format, string(content.Format)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.botSet && f.bot != m.Author.Bot {
		return false
	}
	if f.repSet && f.replies != m.IsReply() {
		return false
	}
	return true
}

// captured pulls this plugin's events out of the store and narrows them with
// the chat-specific filters. Paging is applied afterwards, so a limit never
// counts messages the caller excluded.
func (h *apiHandler) captured(w http.ResponseWriter, r *http.Request) ([]Captured, int, int, bool) {
	q, err := coreapi.ParseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, 0, 0, false
	}
	// This is the chat plugin whatever the caller asked for.
	q.Plugin = PluginName
	limit, offset := q.Limit, q.Offset
	q.Limit, q.Offset = 0, 0

	events, err := h.deps.Store.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, 0, 0, false
	}

	f := parseFilters(r)
	out := make([]Captured, 0, len(events))
	for _, c := range Messages(events) {
		if !f.match(c) {
			continue
		}
		out = append(out, c)
	}
	return out, limit, offset, true
}

func (h *apiHandler) list(w http.ResponseWriter, r *http.Request) {
	captured, limit, offset, ok := h.captured(w, r)
	if !ok {
		return
	}
	out := make([]MessageEnvelope, 0, len(captured))
	for _, c := range captured {
		v := NewMessageEnvelope(c)
		v.URL = coreapi.EventURL(r, c.Event.ID)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, page(out, limit, offset))
}

// channels serves the derived channel index: the same grouping the tab renders,
// so a client scripting against tommy sees exactly what a human does.
func (h *apiHandler) channels(w http.ResponseWriter, r *http.Request) {
	captured, limit, offset, ok := h.captured(w, r)
	if !ok {
		return
	}
	events := make([]*event.Event, 0, len(captured))
	for _, c := range captured {
		events = append(events, c.Event)
	}
	channels := Channels(events)
	out := make([]ChannelSummary, 0, len(channels))
	for _, c := range channels {
		out = append(out, NewChannelSummary(c))
	}
	writeJSON(w, http.StatusOK, page(out, limit, offset))
}

func page[T any](in []T, limit, offset int) []T {
	if offset >= len(in) {
		return []T{}
	}
	if offset > 0 {
		in = in[offset:]
	}
	if limit > 0 && limit < len(in) {
		in = in[:limit]
	}
	return in
}

func (h *apiHandler) get(w http.ResponseWriter, r *http.Request) {
	e, err := h.deps.Store.Get(r.Context(), event.ID(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if e.Plugin != PluginName {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	m, ok := MessageOf(e)
	if !ok {
		writeError(w, http.StatusNotFound, "event "+string(e.ID)+" carries no chat message")
		return
	}
	v := NewMessageEnvelope(Captured{Event: e, Message: m})
	v.URL = coreapi.EventURL(r, e.ID)
	writeJSON(w, http.StatusOK, v)
}

func (h *apiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Store.Clear(r.Context(), PluginName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
