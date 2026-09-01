package slack

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/can3p/tommy/plugins/chat"
)

// content is the parts of a Slack message that both surfaces convert into the
// canonical model, before either one's own metadata is attached.
type content struct {
	Text        string
	Channel     string // raw channel value: a "C…" id, a "#name" or bare name override
	Username    string
	IconURL     string
	IconEmoji   string
	ThreadTS    string
	Blocks      json.RawMessage
	Attachments json.RawMessage
	Mrkdwn      *bool
	UnfurlLinks *bool
	UnfurlMedia *bool
}

// hasBody reports whether the payload carries something worth posting: text,
// blocks or attachments. Slack's own "no_text" error fires when none of the
// three are present.
func (c content) hasBody() bool {
	return strings.TrimSpace(c.Text) != "" || rawHasValue(c.Blocks) || rawHasValue(c.Attachments)
}

// rawHasValue reports whether a json.RawMessage carries a real value rather
// than being empty or the JSON literal null.
func rawHasValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// channelRef turns a raw Slack channel value into a chat.ChannelRef. A "C…"
// id or a bare name is used as-is for ID; a "#name" override keeps the "#" in
// ID (so two different overrides can never collide with a real id) and also
// carries the bare name as Name for display.
func channelRef(raw string) chat.ChannelRef {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return chat.ChannelRef{}
	}
	ref := chat.ChannelRef{ID: raw}
	if strings.HasPrefix(raw, "#") {
		ref.Name = strings.TrimPrefix(raw, "#")
	}
	return ref
}

// webhookChannelRef is channelRef with a fallback: an incoming webhook has no
// channel id of its own when the payload names none, since Slack itself binds
// the destination channel to the webhook URL at install time and never tells
// the poster what it is. team/bot identify that binding well enough to give
// every post through this webhook the same, stable channel identity.
func webhookChannelRef(rawChannel, team, bot string) chat.ChannelRef {
	if ref := channelRef(rawChannel); ref.ID != "" {
		return ref
	}
	return chat.ChannelRef{ID: "webhook:" + team + "/" + bot}
}

// buildMessage converts a parsed payload into the canonical model. bot is
// always true: both surfaces this provider mounts post as a bot or an
// incoming webhook, never as a human.
func buildMessage(c content, channel chat.ChannelRef, ts string) *chat.Message {
	msg := &chat.Message{
		Channel:  channel,
		Author:   chat.Author{Name: c.Username, IconURL: c.IconURL, Bot: true},
		Text:     c.Text,
		TS:       ts,
		ThreadTS: c.ThreadTS,
	}
	if rawHasValue(c.Blocks) {
		msg.Contents = append(msg.Contents, chat.Content{Format: chat.FormatSlackBlocks, Data: c.Blocks})
	}
	if rawHasValue(c.Attachments) {
		msg.Contents = append(msg.Contents, chat.Content{Format: chat.FormatSlackAttachments, Data: c.Attachments})
	}
	return msg
}

// mintTS mints a Slack-shaped message ts ("1503435956.000247": unix seconds, a
// dot, a six-digit fractional counter) for a chat.postMessage call, which is
// the one surface where Slack hands the poster back an identity to keep. The
// fractional part is derived from id rather than a process-global counter, so
// the same (id, now) pair - what plugintest's deterministic Deps hands every
// test - always mints the same ts.
func mintTS(id string, now time.Time) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	frac := h.Sum32() % 1000000
	return fmt.Sprintf("%d.%06d", now.Unix(), frac)
}

// boolMeta adds key to m only when v is non-nil, so an unset override never
// shows up as a false one.
func boolMeta(m map[string]any, key string, v *bool) {
	if v != nil {
		m[key] = *v
	}
}

// strMeta adds key to m only when v is non-empty.
func strMeta(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}
