package sms_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/sms"
)

func TestMessageNormalize(t *testing.T) {
	tests := []struct {
		name          string
		in            sms.Message
		wantStatus    sms.Status
		wantDirection sms.Direction
		wantFrom      string
		wantSegments  sms.Segments
	}{{
		name:          "an outbound message defaults to queued",
		in:            sms.Message{From: "+15005550006", To: "+15551234567", Body: "hi"},
		wantStatus:    sms.StatusQueued,
		wantDirection: sms.Outbound,
		wantFrom:      "+15005550006",
		wantSegments:  sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 2, Capacity: 160, Remaining: 158},
	}, {
		name:          "an inbound message defaults to received",
		in:            sms.Message{From: "+15551234567", To: "+15005550006", Body: "hi", Direction: sms.Inbound},
		wantStatus:    sms.StatusReceived,
		wantDirection: sms.Inbound,
		wantFrom:      "+15551234567",
		wantSegments:  sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 2, Capacity: 160, Remaining: 158},
	}, {
		name:          "an explicit status is kept",
		in:            sms.Message{To: "+15551234567", Body: "hi", Status: sms.StatusDelivered},
		wantStatus:    sms.StatusDelivered,
		wantDirection: sms.Outbound,
		wantSegments:  sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 2, Capacity: 160, Remaining: 158},
	}, {
		name:          "surrounding whitespace is trimmed off the numbers",
		in:            sms.Message{From: "  +15005550006 ", To: "\t+15551234567\n", Body: "hi"},
		wantStatus:    sms.StatusQueued,
		wantDirection: sms.Outbound,
		wantFrom:      "+15005550006",
		wantSegments:  sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 2, Capacity: 160, Remaining: 158},
	}, {
		name: "segments supplied by a provider are recomputed, never trusted",
		in: sms.Message{
			To:       "+15551234567",
			Body:     strings.Repeat("a", 161),
			Segments: sms.Segments{Count: 99, Encoding: sms.UCS2},
		},
		wantStatus:    sms.StatusQueued,
		wantDirection: sms.Outbound,
		wantSegments:  sms.Segments{Count: 2, Encoding: sms.GSM7, Units: 161, Capacity: 153, Remaining: 145},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.in
			m.Normalize()
			if m.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", m.Status, tt.wantStatus)
			}
			if m.Direction != tt.wantDirection {
				t.Errorf("Direction = %q, want %q", m.Direction, tt.wantDirection)
			}
			if tt.wantFrom != "" && m.From != tt.wantFrom {
				t.Errorf("From = %q, want %q", m.From, tt.wantFrom)
			}
			if m.Segments != tt.wantSegments {
				t.Errorf("Segments = %#v, want %#v", m.Segments, tt.wantSegments)
			}
		})
	}
}

func TestMessageEndpoints(t *testing.T) {
	tests := []struct {
		name                            string
		in                              sms.Message
		wantSender, wantPeer, wantLocal string
	}{{
		name:       "outbound: the peer is the destination",
		in:         sms.Message{From: "+15005550006", To: "+15551234567"},
		wantSender: "+15005550006", wantPeer: "+15551234567", wantLocal: "+15005550006",
	}, {
		name:       "inbound: the peer is the sender",
		in:         sms.Message{From: "+15551234567", To: "+15005550006", Direction: sms.Inbound},
		wantSender: "+15551234567", wantPeer: "+15551234567", wantLocal: "+15005550006",
	}, {
		name:       "a messaging service stands in for a missing From",
		in:         sms.Message{MessagingService: "MG0123", To: "+15551234567"},
		wantSender: "MG0123", wantPeer: "+15551234567", wantLocal: "MG0123",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.in
			m.Normalize()
			if got := m.Sender(); got != tt.wantSender {
				t.Errorf("Sender() = %q, want %q", got, tt.wantSender)
			}
			if got := m.Peer(); got != tt.wantPeer {
				t.Errorf("Peer() = %q, want %q", got, tt.wantPeer)
			}
			if got := m.Local(); got != tt.wantLocal {
				t.Errorf("Local() = %q, want %q", got, tt.wantLocal)
			}
		})
	}
}

func TestMessageEventSummary(t *testing.T) {
	tests := []struct {
		name        string
		in          sms.Message
		wantTitle   string
		wantFrom    string
		wantSnippet string
	}{{
		name:        "the title is the body",
		in:          sms.Message{From: "+1500", To: "+1555", Body: "It works."},
		wantTitle:   "It works.",
		wantFrom:    "+1500",
		wantSnippet: "It works.",
	}, {
		name:        "only the first line becomes the title",
		in:          sms.Message{To: "+1555", Body: "first line\nsecond line"},
		wantTitle:   "first line",
		wantSnippet: "first line\nsecond line",
	}, {
		name:      "an empty body says so",
		in:        sms.Message{To: "+1555"},
		wantTitle: "(empty message)",
	}, {
		name:      "a media-only message says so",
		in:        sms.Message{To: "+1555", Media: []sms.Media{{URL: "https://example.com/cat.png"}}},
		wantTitle: "(media only)",
	}, {
		name:      "a very long body is truncated",
		in:        sms.Message{To: "+1555", Body: strings.Repeat("x", 200)},
		wantTitle: strings.Repeat("x", 79) + "…",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.in
			m.Normalize()
			got := m.EventSummary()
			if got.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tt.wantTitle)
			}
			if tt.wantFrom != "" && got.From != tt.wantFrom {
				t.Errorf("From = %q, want %q", got.From, tt.wantFrom)
			}
			if tt.wantSnippet != "" && got.Snippet != tt.wantSnippet {
				t.Errorf("Snippet = %q, want %q", got.Snippet, tt.wantSnippet)
			}
			if !reflect.DeepEqual(got.To, []string{m.To}) {
				t.Errorf("To = %v, want %v", got.To, []string{m.To})
			}
		})
	}
}

func TestMedia(t *testing.T) {
	tests := []struct {
		name        string
		in          sms.Media
		wantStored  bool
		wantType    string
		wantIsImage bool
		wantSize    int64
		wantName    string
	}{{
		name:        "stored image",
		in:          sms.Media{ContentType: "image/png", Filename: "cat.png", Blob: &blob.Ref{ID: "b1", Size: 12}},
		wantStored:  true,
		wantType:    "image/png",
		wantIsImage: true,
		wantSize:    12,
		wantName:    "cat.png",
	}, {
		name:       "the blob's content type is the fallback",
		in:         sms.Media{Blob: &blob.Ref{ID: "b1", ContentType: "application/pdf", Filename: "receipt.pdf"}},
		wantStored: true,
		wantType:   "application/pdf",
		wantName:   "receipt.pdf",
	}, {
		name:     "a url-only media is not stored and is named after its last path element",
		in:       sms.Media{ContentType: "image/gif", URL: "https://example.com/media/party.gif"},
		wantType: "image/gif", wantIsImage: true, wantName: "party.gif",
	}, {
		name:     "a media with nothing at all still has a name",
		in:       sms.Media{},
		wantName: "attachment",
	}, {
		name:       "a blob with an empty id does not count as stored",
		in:         sms.Media{Blob: &blob.Ref{}},
		wantStored: false,
		wantName:   "attachment",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Stored(); got != tt.wantStored {
				t.Errorf("Stored() = %v, want %v", got, tt.wantStored)
			}
			if got := tt.in.Type(); got != tt.wantType {
				t.Errorf("Type() = %q, want %q", got, tt.wantType)
			}
			if got := tt.in.IsImage(); got != tt.wantIsImage {
				t.Errorf("IsImage() = %v, want %v", got, tt.wantIsImage)
			}
			if got := tt.in.Size(); got != tt.wantSize {
				t.Errorf("Size() = %d, want %d", got, tt.wantSize)
			}
			if got := tt.in.Name(); got != tt.wantName {
				t.Errorf("Name() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestIsE164(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"+15551234567", true},
		{"+1", true},
		{"+123456789012345", true},
		{"+1234567890123456", false}, // 16 digits is one too many
		{"+05551234567", false},      // country codes do not start with zero
		{"15551234567", false},       // no leading plus
		{"+1555123456a", false},
		{"+", false},
		{"", false},
		{"ACME", false},
		{"+1 555 123 4567", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := sms.IsE164(tt.in); got != tt.want {
				t.Errorf("IsE164(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// MessageOf has to survive a payload that has been through JSON, because a
// persistent store added later would hand back exactly that.
func TestMessageOf(t *testing.T) {
	msg := &sms.Message{From: "+15005550006", To: "+15551234567", Body: "hi €"}
	msg.Normalize()

	var asMap map[string]any
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		payload any
		wantOK  bool
	}{
		{"pointer payload", msg, true},
		{"value payload", *msg, true},
		{"payload that went through json", asMap, true},
		{"nil payload", nil, false},
		{"nil typed payload", (*sms.Message)(nil), false},
		{"an unrelated payload", map[string]any{"unrelated": true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sms.MessageOf(&event.Event{Payload: tt.payload})
			if ok != tt.wantOK {
				t.Fatalf("MessageOf ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.To != msg.To || got.Body != msg.Body || got.Segments != msg.Segments {
				t.Errorf("decoded %#v, want %#v", got, msg)
			}
		})
	}

	t.Run("a nil event decodes to nothing", func(t *testing.T) {
		if _, ok := sms.MessageOf(nil); ok {
			t.Error("MessageOf(nil) should not succeed")
		}
	})

	t.Run("the returned message is a copy", func(t *testing.T) {
		e := &event.Event{Payload: msg}
		got, _ := sms.MessageOf(e)
		got.Body = "mutated"
		if msg.Body == "mutated" {
			t.Error("MessageOf handed out the stored message itself; events are immutable once appended")
		}
	})
}

func TestMessagesSkipsForeignEvents(t *testing.T) {
	msg := &sms.Message{To: "+15551234567", Body: "hi"}
	msg.Normalize()
	events := []*event.Event{
		{ID: "1", Type: sms.EventType, Payload: msg},
		{ID: "2", Type: "sms.status", Payload: msg},           // a future resource
		{ID: "3", Type: sms.EventType, Payload: "not an sms"}, // undecodable
	}
	got := sms.Messages(events)
	if len(got) != 1 || got[0].Event.ID != "1" {
		t.Fatalf("Messages() = %d entries, want just the sms.message one", len(got))
	}
}
