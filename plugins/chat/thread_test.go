package chat_test

import (
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/chat"
)

// build turns messages into events the way a provider would, stamping arrival
// times a second apart and handing them back newest first - which is the order
// the store lists them in, and therefore the order the derivation has to cope
// with.
func build(messages ...*chat.Message) []*event.Event {
	events := make([]*event.Event, 0, len(messages))
	for i, m := range messages {
		ev := chat.NewEvent("fake", m)
		ev.ID = event.ID(string(rune('a'+i)) + "0000000000000000000000")
		ev.ReceivedAt = at(i)
		events = append(events, ev)
	}
	// Reverse: newest first, as core/store lists them.
	out := make([]*event.Event, len(events))
	for i, e := range events {
		out[len(events)-1-i] = e
	}
	return out
}

func channelKeys(chans []*chat.Channel) []string {
	out := make([]string, len(chans))
	for i, c := range chans {
		out[i] = c.Key
	}
	return out
}

func texts(items []chat.Captured) []string {
	out := make([]string, len(items))
	for i, c := range items {
		out[i] = c.Message.Text
	}
	return out
}

func TestChannelsGroupsAndOrders(t *testing.T) {
	channels := chat.Channels(build(
		msg("C-ops", "deploy-bot", "ops first", "1.1"),
		msg("C-general", "deploy-bot", "general first", "2.1"),
		msg("C-general", "deploy-bot", "general second", "2.2"),
	))

	// Newest activity first, the way a chat sidebar sorts.
	if got := channelKeys(channels); len(got) != 2 || got[0] != "C-general" || got[1] != "C-ops" {
		t.Fatalf("channel order = %v, want the most recently active first", got)
	}
	if channels[0].Count() != 2 || channels[1].Count() != 1 {
		t.Errorf("counts = %d, %d", channels[0].Count(), channels[1].Count())
	}
	last, ok := channels[0].Last()
	if !ok || last.Message.Text != "general second" {
		t.Errorf("Last() = %+v, want the newest message", last.Message)
	}
	if chat.FindChannel(channels, "C-ops") == nil {
		t.Error("FindChannel must find a channel by key")
	}
	if chat.FindChannel(channels, "nope") != nil {
		t.Error("FindChannel must return nil for an unknown key")
	}
}

func TestChannelPicksUpADisplayName(t *testing.T) {
	channels := chat.Channels(build(
		msg("C0123ABCD", "bot", "first", "1.1"),
		func() *chat.Message {
			m := msg("C0123ABCD", "bot", "second", "1.2")
			m.Channel.Name = "general"
			return m
		}(),
	))
	if len(channels) != 1 {
		t.Fatalf("got %d channels, want the id to group them into one", len(channels))
	}
	if channels[0].Display() != "general" {
		t.Errorf("Display() = %q, want the name the later message carried", channels[0].Display())
	}
	if channels[0].ID != "C0123ABCD" {
		t.Errorf("ID = %q", channels[0].ID)
	}
}

func TestThreadNestsRepliesUnderTheirParent(t *testing.T) {
	channels := chat.Channels(build(
		msg("C1", "bot", "root a", "1.1"),
		msg("C1", "bot", "root b", "1.2"),
		reply("C1", "bot", "reply a1", "1.3", "1.1"),
		reply("C1", "bot", "reply a2", "1.4", "1.1"),
		reply("C1", "bot", "reply b1", "1.5", "1.2"),
	))
	if len(channels) != 1 {
		t.Fatalf("got %d channels, want 1", len(channels))
	}
	ch := channels[0]
	if ch.Count() != 5 || ch.ThreadCount() != 2 || ch.ReplyCount() != 3 {
		t.Fatalf("channel = %d messages, %d threads, %d replies", ch.Count(), ch.ThreadCount(), ch.ReplyCount())
	}

	// Threads sit where their root was posted, so a late reply does not drag a
	// thread to the bottom of the stream.
	first, second := ch.Threads[0], ch.Threads[1]
	if first.Root == nil || first.Root.Message.Text != "root a" {
		t.Fatalf("first thread root = %+v", first.Root)
	}
	if got := texts(first.Replies); len(got) != 2 || got[0] != "reply a1" || got[1] != "reply a2" {
		t.Errorf("replies = %v, want them oldest first", got)
	}
	if second.Root == nil || second.Root.Message.Text != "root b" || len(second.Replies) != 1 {
		t.Errorf("second thread = %+v", second)
	}
	if got := texts(first.Messages()); len(got) != 3 || got[0] != "root a" {
		t.Errorf("Messages() = %v, want the root then its replies", got)
	}
	if first.Orphan() || first.Count() != 3 {
		t.Errorf("thread orphan=%v count=%d", first.Orphan(), first.Count())
	}
	if ch.Orphans() != 0 {
		t.Errorf("Orphans() = %d, want 0", ch.Orphans())
	}
}

// A reply whose parent was never captured - posted before tommy started, or
// evicted from the ring buffer - is guaranteed to happen. It must not lose the
// replies and it must not break the derivation.
func TestOrphanedReplyIsKept(t *testing.T) {
	channels := chat.Channels(build(
		msg("C1", "bot", "a normal root", "1.1"),
		reply("C1", "bot", "orphan one", "9.1", "0.9"),
		reply("C1", "bot", "orphan two", "9.2", "0.9"),
	))
	ch := channels[0]
	if ch.Count() != 3 || ch.ThreadCount() != 2 {
		t.Fatalf("channel = %d messages in %d threads", ch.Count(), ch.ThreadCount())
	}
	if ch.Orphans() != 1 {
		t.Fatalf("Orphans() = %d, want 1", ch.Orphans())
	}

	var orphan *chat.Thread
	for _, th := range ch.Threads {
		if th.Orphan() {
			orphan = th
		}
	}
	if orphan == nil {
		t.Fatal("the orphaned thread disappeared")
	}
	if orphan.Root != nil {
		t.Error("an orphaned thread has no root")
	}
	if orphan.RootID != "0.9" {
		t.Errorf("RootID = %q, want the parent the replies point at", orphan.RootID)
	}
	if got := texts(orphan.Replies); len(got) != 2 || got[0] != "orphan one" {
		t.Errorf("orphan replies = %v", got)
	}
	if orphan.Count() != 2 {
		t.Errorf("Count() = %d, want just the replies", orphan.Count())
	}
	if got := texts(orphan.Messages()); len(got) != 2 || got[0] != "orphan one" {
		t.Errorf("Messages() = %v, want the surviving replies", got)
	}
	if orphan.Anchor != at(1) {
		t.Errorf("Anchor = %v, want the earliest surviving reply at %v", orphan.Anchor, at(1))
	}
}

// The root may be listed after its replies - a backdated event, or simply the
// order the store hands things back. Two passes, so it still nests.
func TestRootArrivingAfterItsRepliesStillNests(t *testing.T) {
	root := msg("C1", "bot", "the root", "1.1")
	rep := reply("C1", "bot", "the reply", "1.2", "1.1")

	// The reply arrives first.
	events := build(rep, root)
	ch := chat.Channels(events)[0]
	if ch.ThreadCount() != 1 {
		t.Fatalf("got %d threads, want the reply and its root in one", ch.ThreadCount())
	}
	th := ch.Threads[0]
	if th.Orphan() {
		t.Fatal("the thread must not be orphaned once the root shows up")
	}
	if th.Root.Message.Text != "the root" || len(th.Replies) != 1 {
		t.Errorf("thread = %+v", th)
	}
}

// A Teams incoming webhook post has no ts of its own, so each message is
// identified by its event id. They must not all collapse into one thread.
func TestMessagesWithoutATimestampGetTheirOwnThread(t *testing.T) {
	a := &chat.Message{Channel: chat.ChannelRef{ID: "webhook"}, Text: "one"}
	b := &chat.Message{Channel: chat.ChannelRef{ID: "webhook"}, Text: "two"}
	ch := chat.Channels(build(a, b))[0]
	if ch.ThreadCount() != 2 {
		t.Fatalf("got %d threads, want one per message", ch.ThreadCount())
	}
	if ch.Threads[0].RootID == ch.Threads[1].RootID {
		t.Error("two messages without a ts must not share an identity")
	}
}

func TestDuplicateRootIdentityKeepsBothMessages(t *testing.T) {
	// Two top-level messages claiming the same ts: nothing may be dropped.
	ch := chat.Channels(build(
		msg("C1", "bot", "first", "1.1"),
		msg("C1", "bot", "second", "1.1"),
	))[0]
	if ch.Count() != 2 {
		t.Fatalf("Count() = %d, want both messages", ch.Count())
	}
	if ch.ThreadCount() != 1 {
		t.Fatalf("ThreadCount() = %d, want one thread for the shared identity", ch.ThreadCount())
	}
	if got := texts(ch.Threads[0].Messages()); len(got) != 2 || got[0] != "first" {
		t.Errorf("thread = %v, want both, first one as the root", got)
	}
}

// The same thread_ts in two channels is two separate threads: grouping is by
// channel first, so a reply never lands in somebody else's stream.
func TestSameThreadTSInTwoChannelsStaysSeparate(t *testing.T) {
	channels := chat.Channels(build(
		msg("C1", "bot", "c1 root", "1.1"),
		msg("C2", "bot", "c2 root", "1.1"),
		reply("C2", "bot", "c2 reply", "1.2", "1.1"),
	))
	if len(channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(channels))
	}
	for _, ch := range channels {
		if ch.ThreadCount() != 1 {
			t.Errorf("channel %s has %d threads, want 1", ch.Key, ch.ThreadCount())
		}
		if ch.ID == "C1" && ch.Count() != 1 {
			t.Errorf("C1 picked up %d messages, want 1", ch.Count())
		}
		if ch.ID == "C2" && ch.Count() != 2 {
			t.Errorf("C2 has %d messages, want 2", ch.Count())
		}
	}
}

func TestChannelsIsOrderIndependent(t *testing.T) {
	messages := []*chat.Message{
		msg("C1", "bot", "root", "1.1"),
		reply("C1", "bot", "r1", "1.2", "1.1"),
		msg("C2", "bot", "other", "2.1"),
		reply("C1", "bot", "r2", "1.3", "1.1"),
	}
	want := chat.Channels(build(messages...))

	// Feed the same events in arrival order rather than newest first.
	events := build(messages...)
	shuffled := make([]*event.Event, len(events))
	for i, e := range events {
		shuffled[len(events)-1-i] = e
	}
	got := chat.Channels(shuffled)

	if len(got) != len(want) {
		t.Fatalf("got %d channels, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Key != want[i].Key || got[i].Count() != want[i].Count() {
			t.Fatalf("channel %d differs: %+v vs %+v", i, got[i], want[i])
		}
		if texts(got[i].Threads[0].Messages())[0] != texts(want[i].Threads[0].Messages())[0] {
			t.Errorf("thread contents differ for %s", got[i].Key)
		}
	}
}

func TestChannelsOnAnEmptyList(t *testing.T) {
	if got := chat.Channels(nil); len(got) != 0 {
		t.Errorf("Channels(nil) = %v", got)
	}
	if got := chat.Channels([]*event.Event{{Plugin: "chat", Type: "chat.other"}}); len(got) != 0 {
		t.Errorf("an unrelated event must not produce a channel: %v", got)
	}
}

func TestThreadKeysAreUniqueAndSafe(t *testing.T) {
	ch := chat.Channels(build(
		msg("#general", "bot", "one", "1.1"),
		msg("#general", "bot", "two", "1.2"),
	))[0]
	seen := map[string]bool{}
	for _, th := range ch.Threads {
		if seen[th.Key] {
			t.Fatalf("duplicate thread key %q", th.Key)
		}
		seen[th.Key] = true
	}
	if ch.Key != chat.ChannelKey("#general") {
		t.Errorf("channel key = %q", ch.Key)
	}
	if want := chat.ThreadKey(ch.Key, "1.1"); !seen[want] {
		t.Errorf("thread keys %v do not include %q", seen, want)
	}
}

// Ties in ReceivedAt are broken by arrival order, which is what keeps the
// stream stable when a burst of messages shares a timestamp.
func TestMessagesSharingATimestampKeepArrivalOrder(t *testing.T) {
	var events []*event.Event
	for i, text := range []string{"one", "two", "three"} {
		ev := chat.NewEvent("fake", msg("C1", "bot", text, ""))
		ev.ID = event.ID(string(rune('a'+i)) + "000000000000000000000")
		ev.ReceivedAt = base
		events = append(events, ev)
	}
	// Newest first with sequence breaking ties, as the store returns them.
	newestFirst := []*event.Event{events[2], events[1], events[0]}
	ch := chat.Channels(newestFirst)[0]
	var got []string
	for _, th := range ch.Threads {
		got = append(got, th.Root.Message.Text)
	}
	if len(got) != 3 || got[0] != "one" || got[2] != "three" {
		t.Errorf("stream order = %v, want arrival order", got)
	}
}
