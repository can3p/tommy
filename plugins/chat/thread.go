package chat

import (
	"sort"
	"time"

	"github.com/can3p/tommy/core/event"
)

// Channel is every message posted to one destination, which is the unit the
// chat tab renders in its sidebar: a chat client shows you a list of channels
// and a stream inside the one you picked, not a flat log.
//
// It is derived from the flat event list every time something is rendered.
// Nothing about it is stored: threads are a relation and core/store has none on
// purpose, so the threading logic lives in the one plugin that needs it.
type Channel struct {
	// Key identifies the channel in a URL and is stable as long as the
	// provider's channel id is.
	Key string
	// ID is the provider's own channel identifier.
	ID string
	// Name is the display name, taken from the most recent message that
	// carried one.
	Name string
	// Threads are the conversations in the channel, oldest root first, the way
	// a chat client stacks them.
	Threads []*Thread
	// Latest is when the newest message in the channel arrived.
	Latest time.Time

	total int
	last  Captured
}

// Display is what the channel is called in the sidebar.
func (c *Channel) Display() string {
	return ChannelRef{ID: c.ID, Name: c.Name}.Display()
}

// Count is how many messages the channel holds, replies included.
func (c *Channel) Count() int { return c.total }

// ThreadCount is how many threads the channel holds.
func (c *Channel) ThreadCount() int { return len(c.Threads) }

// ReplyCount is how many of the channel's messages are replies.
func (c *Channel) ReplyCount() int {
	n := 0
	for _, t := range c.Threads {
		n += len(t.Replies)
	}
	return n
}

// Orphans is how many of the channel's threads reply to a parent that was never
// captured. It is worth surfacing: it usually means the parent was posted
// before tommy started, or has been evicted from the ring buffer.
func (c *Channel) Orphans() int {
	n := 0
	for _, t := range c.Threads {
		if t.Orphan() {
			n++
		}
	}
	return n
}

// Last is the newest message in the channel, for the sidebar preview.
func (c *Channel) Last() (Captured, bool) {
	if c.last.Event == nil {
		return Captured{}, false
	}
	return c.last, true
}

// Thread is a root message and the replies that point at it.
//
// Root is nil when the parent was never captured or has been evicted from the
// ring buffer, which will happen and must not lose the replies: the thread is
// still rendered, marked as orphaned, with everything that did arrive.
type Thread struct {
	// Key identifies the thread inside its channel, for an anchor or a
	// fragment id.
	Key string
	// RootID is the identity every message in the thread points at: the parent
	// message's ts, or the event id of a parent that had none.
	RootID string
	// Root is the parent message, or nil when it was never captured.
	Root *Captured
	// Replies are the messages hanging under the root, oldest first.
	Replies []Captured
	// Anchor is where the thread sits in the channel stream: when the root was
	// posted, or when the earliest surviving reply was, for an orphan. Using
	// the root rather than the latest activity is what keeps a thread from
	// jumping to the bottom of the stream every time somebody replies.
	Anchor time.Time
	// Latest is when the newest message in the thread arrived.
	Latest time.Time
}

// Orphan reports whether the thread's parent message is missing.
func (t *Thread) Orphan() bool { return t.Root == nil }

// Count is how many captured messages the thread holds.
func (t *Thread) Count() int {
	if t.Root == nil {
		return len(t.Replies)
	}
	return len(t.Replies) + 1
}

// Messages returns the thread in reading order: the root first, then its
// replies. An orphaned thread returns just the replies.
func (t *Thread) Messages() []Captured {
	out := make([]Captured, 0, t.Count())
	if t.Root != nil {
		out = append(out, *t.Root)
	}
	return append(out, t.Replies...)
}

// ChannelKey is the URL-safe identifier of a channel, derived from the
// provider's channel id.
func ChannelKey(id string) string { return Slug(id) }

// ThreadKey is the identifier of a thread inside a channel.
func ThreadKey(channelKey, rootID string) string { return channelKey + "~" + Slug(rootID) }

// Channels derives the channel and thread index from a flat event list, newest
// channel first. Events that carry no decodable chat message are skipped.
//
// The derivation is the whole of the threading logic:
//
//  1. messages are grouped by channel id;
//  2. inside a channel each message is filed under its root key - its parent's
//     ts when it is a reply, its own identity otherwise;
//  3. a thread's root is the non-reply message whose identity is that key, and
//     a thread whose root never arrived is kept anyway, marked orphaned.
//
// Order is stable and does not depend on the order events came out of the
// store: messages are sorted oldest first, threads sit at their root's
// timestamp, and channels are listed by most recent activity.
func Channels(events []*event.Event) []*Channel {
	captured := oldestFirst(Messages(events))

	byKey := map[string]*Channel{}
	items := map[string][]Captured{}
	var order []*Channel

	for _, c := range captured {
		key := ChannelKey(c.Message.Channel.ID)
		ch, ok := byKey[key]
		if !ok {
			ch = &Channel{Key: key, ID: c.Message.Channel.ID}
			byKey[key] = ch
			order = append(order, ch)
		}
		// The newest message that names the channel wins, so a channel first
		// seen by id picks up a display name as soon as one arrives.
		if c.Message.Channel.Name != "" {
			ch.Name = c.Message.Channel.Name
		}
		ch.total++
		items[key] = append(items[key], c)
		if !c.Event.ReceivedAt.Before(ch.Latest) {
			ch.Latest = c.Event.ReceivedAt
			ch.last = c
		}
	}

	for _, ch := range order {
		ch.Threads = buildThreads(ch.Key, items[ch.Key])
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Latest.After(order[j].Latest) })
	return order
}

// buildThreads nests one channel's messages, which arrive oldest first.
func buildThreads(channelKey string, items []Captured) []*Thread {
	byRoot := map[string]*Thread{}
	var order []*Thread

	for _, c := range items {
		rootID := c.RootKey()
		t, ok := byRoot[rootID]
		if !ok {
			t = &Thread{Key: ThreadKey(channelKey, rootID), RootID: rootID}
			byRoot[rootID] = t
			order = append(order, t)
		}
		switch {
		case c.Message.IsReply():
			t.Replies = append(t.Replies, c)
		case t.Root == nil:
			t.Root = &Captured{Event: c.Event, Message: c.Message}
		default:
			// Two top-level messages claiming the same identity: keep the
			// first as the root and hang the rest under it rather than
			// dropping one on the floor.
			t.Replies = append(t.Replies, c)
		}
		if c.Event.ReceivedAt.After(t.Latest) {
			t.Latest = c.Event.ReceivedAt
		}
	}

	for _, t := range order {
		switch {
		case t.Root != nil:
			t.Anchor = t.Root.Event.ReceivedAt
		case len(t.Replies) > 0:
			// The parent was never captured, or has been evicted. The thread
			// still renders; it just sits where its earliest surviving reply
			// landed.
			t.Anchor = t.Replies[0].Event.ReceivedAt
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Anchor.Before(order[j].Anchor) })
	return order
}

// FindChannel returns the channel with the given key.
func FindChannel(channels []*Channel, key string) *Channel {
	for _, c := range channels {
		if c.Key == key {
			return c
		}
	}
	return nil
}

// oldestFirst returns the messages in arrival order. The store lists newest
// first with arrival order breaking ties, so the slice is reversed before it is
// sorted: a stable sort alone would keep messages that share a timestamp in
// reverse order.
func oldestFirst(in []Captured) []Captured {
	out := make([]Captured, len(in))
	for i, c := range in {
		out[len(in)-1-i] = c
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Event.ReceivedAt.Before(out[j].Event.ReceivedAt)
	})
	return out
}
