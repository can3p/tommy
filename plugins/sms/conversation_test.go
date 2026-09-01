package sms_test

import (
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/sms"
)

func at(seconds int) time.Time {
	return time.Date(2024, 1, 1, 12, 0, seconds, 0, time.UTC)
}

func ev(id string, seconds int, m *sms.Message) *event.Event {
	m.Normalize()
	return &event.Event{
		ID:         event.ID(id),
		Plugin:     sms.Name,
		Provider:   "fake",
		Type:       sms.EventType,
		ReceivedAt: at(seconds),
		Summary:    m.EventSummary(),
		Payload:    m,
	}
}

func TestConversationKeyIsSymmetricAndURLSafe(t *testing.T) {
	tests := []struct {
		name      string
		a, b      string
		c, d      string
		wantEqual bool
	}{
		{"the same pair in either order", "+15005550006", "+15551234567", "+15551234567", "+15005550006", true},
		{"a different peer is a different thread", "+15005550006", "+15551234567", "+15005550006", "+15559999999", false},
		{"an alphanumeric sender id works too", "ACME", "+15551234567", "+15551234567", "ACME", true},
		{"an empty endpoint still yields a key", "", "+15551234567", "+15551234567", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			one := sms.ConversationKey(tt.a, tt.b)
			two := sms.ConversationKey(tt.c, tt.d)
			if (one == two) != tt.wantEqual {
				t.Errorf("ConversationKey(%q,%q)=%q vs (%q,%q)=%q, wantEqual=%v", tt.a, tt.b, one, tt.c, tt.d, two, tt.wantEqual)
			}
			for _, r := range one {
				ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.'
				if !ok {
					t.Errorf("key %q contains %q, which would need escaping in a URL path", one, r)
				}
			}
		})
	}
}

func TestConversationsGrouping(t *testing.T) {
	local, peerA, peerB := "+15005550006", "+15551234567", "+15559999999"

	events := []*event.Event{
		ev("e4", 40, &sms.Message{From: local, To: peerB, Body: "other thread"}),
		ev("e3", 30, &sms.Message{From: peerA, To: local, Body: "reply", Direction: sms.Inbound}),
		ev("e2", 20, &sms.Message{From: local, To: peerA, Body: "second"}),
		ev("e1", 10, &sms.Message{From: local, To: peerA, Body: "first"}),
	}

	convs := sms.Conversations(events)
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}

	// Newest thread first: peerB spoke last.
	if convs[0].Peer != peerB {
		t.Errorf("first conversation peer = %q, want %q", convs[0].Peer, peerB)
	}
	if convs[0].Latest != at(40) {
		t.Errorf("first conversation Latest = %v, want %v", convs[0].Latest, at(40))
	}

	thread := convs[1]
	if thread.Peer != peerA || thread.Local != local {
		t.Errorf("thread endpoints = peer %q / local %q, want %q / %q", thread.Peer, thread.Local, peerA, local)
	}
	if thread.Count() != 3 {
		t.Fatalf("thread has %d messages, want 3: both directions belong to one conversation", thread.Count())
	}
	// Oldest first, the way a phone stacks them.
	wantOrder := []string{"first", "second", "reply"}
	for i, want := range wantOrder {
		if got := thread.Items[i].Message.Body; got != want {
			t.Errorf("message %d = %q, want %q", i, got, want)
		}
	}
	if last := thread.Last(); last == nil || last.Body != "reply" {
		t.Errorf("Last() = %v, want the inbound reply", last)
	}
	// The newest message decides which end is which, so an inbound last message
	// must not flip the thread inside out.
	if thread.Peer != peerA {
		t.Errorf("after an inbound reply the peer is %q, want %q", thread.Peer, peerA)
	}

	if got := sms.FindConversation(convs, thread.Key); got != thread {
		t.Errorf("FindConversation did not return the thread it was given the key of")
	}
	if got := sms.FindConversation(convs, "nope"); got != nil {
		t.Errorf("FindConversation(unknown) = %v, want nil", got)
	}
}

func TestConversationsMessagingServiceThread(t *testing.T) {
	convs := sms.Conversations([]*event.Event{
		ev("e1", 10, &sms.Message{MessagingService: "MG0123", To: "+15551234567", Body: "hi"}),
	})
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convs))
	}
	if convs[0].Local != "MG0123" {
		t.Errorf("Local = %q, want the messaging service", convs[0].Local)
	}
	if convs[0].Title() != "+15551234567" {
		t.Errorf("Title() = %q, want the peer", convs[0].Title())
	}
}

func TestConversationTitleFallbacks(t *testing.T) {
	empty := &sms.Conversation{}
	if got := empty.Title(); got != "(unknown)" {
		t.Errorf("Title() = %q, want %q", got, "(unknown)")
	}
	if empty.Last() != nil {
		t.Error("Last() on an empty conversation should be nil")
	}
	local := &sms.Conversation{Local: "+15005550006"}
	if got := local.Title(); got != "+15005550006" {
		t.Errorf("Title() = %q, want the local endpoint when there is no peer", got)
	}
}

func TestConversationsIgnoresUndecodableEvents(t *testing.T) {
	convs := sms.Conversations([]*event.Event{
		{ID: "x", Type: sms.EventType, ReceivedAt: at(1), Payload: "not a message"},
	})
	if len(convs) != 0 {
		t.Fatalf("got %d conversations, want 0", len(convs))
	}
}
