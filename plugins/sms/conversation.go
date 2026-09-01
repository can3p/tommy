package sms

import (
	"sort"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
)

// Conversation is every message exchanged between one pair of endpoints, which
// is the unit the SMS tab renders: a phone shows you a thread per correspondent,
// not a flat log.
type Conversation struct {
	// Key identifies the thread in a URL. It is derived from the endpoint pair
	// and is stable as long as the pair is.
	Key string
	// Peer is the far end - the number the application under test is texting.
	Peer string
	// Local is the near end: the application's own number or messaging service.
	Local string
	// Items are the messages, oldest first, the way a phone stacks them.
	Items []Captured
	// Latest is when the newest message arrived.
	Latest time.Time
}

// Count is how many messages the thread holds.
func (c *Conversation) Count() int { return len(c.Items) }

// Last is the newest message in the thread, or nil for an empty one.
func (c *Conversation) Last() *Message {
	if len(c.Items) == 0 {
		return nil
	}
	return c.Items[len(c.Items)-1].Message
}

// Title is what the thread is called in the list: the peer, falling back to the
// local endpoint when a provider gave us only one side.
func (c *Conversation) Title() string {
	if c.Peer != "" {
		return c.Peer
	}
	if c.Local != "" {
		return c.Local
	}
	return "(unknown)"
}

// Conversations groups events into threads, newest thread first. Events that do
// not carry a decodable message are skipped.
func Conversations(events []*event.Event) []*Conversation {
	byKey := map[string]*Conversation{}
	var order []*Conversation

	for _, captured := range Messages(events) {
		m := captured.Message
		key := ConversationKey(m.Local(), m.Peer())
		conv, ok := byKey[key]
		if !ok {
			conv = &Conversation{Key: key}
			byKey[key] = conv
			order = append(order, conv)
		}
		conv.Items = append(conv.Items, captured)
		if captured.Event.ReceivedAt.After(conv.Latest) {
			conv.Latest = captured.Event.ReceivedAt
			// The newest message decides which end is which, so a thread that
			// started inbound and continued outbound still reads correctly.
			conv.Peer, conv.Local = m.Peer(), m.Local()
		}
	}

	for _, conv := range order {
		items := conv.Items
		sort.SliceStable(items, func(i, j int) bool {
			return items[i].Event.ReceivedAt.Before(items[j].Event.ReceivedAt)
		})
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Latest.After(order[j].Latest) })
	return order
}

// FindConversation returns the thread with the given key.
func FindConversation(convs []*Conversation, key string) *Conversation {
	for _, c := range convs {
		if c.Key == key {
			return c
		}
	}
	return nil
}

// ConversationKey builds the URL-safe identifier of the thread between two
// endpoints. It is symmetric, so both directions land in the same thread, and
// every character outside [A-Za-z0-9] is replaced, so a "+" or an alphanumeric
// sender id never needs escaping in a path.
func ConversationKey(local, peer string) string {
	a, b := local, peer
	if a > b {
		a, b = b, a
	}
	return slug(a) + "." + slug(b)
}

func slug(s string) string {
	if s == "" {
		return "none"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
