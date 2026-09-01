package sms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/sms"
)

// onePixelPNG is a real, tiny PNG, so the media route is asserted against bytes
// a browser would actually accept.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
	0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func listMessages(t *testing.T, in *testutil.Instance, query string) []sms.MessageEnvelope {
	t.Helper()
	var out []sms.MessageEnvelope
	url := in.API("/sms/messages")
	if query != "" {
		url += "?" + query
	}
	if status := in.GetJSON(url, &out); status != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, status)
	}
	return out
}

func putBlob(t *testing.T, in *testutil.Instance, contentType, filename string, data []byte) blob.Ref {
	t.Helper()
	ref, err := in.Blobs.Put(context.Background(), bytes.NewReader(data), blob.Ref{
		ContentType: contentType,
		Filename:    filename,
	})
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	return ref
}

func TestAPIListEmpty(t *testing.T) {
	in := start(t)
	got := listMessages(t, in, "")
	if len(got) != 0 {
		t.Fatalf("got %d messages from an empty tommy, want 0", len(got))
	}
	// An empty listing must be [] rather than null, so a client can iterate it.
	if _, body := in.GetBody(in.API("/sms/messages")); strings.TrimSpace(body) != "[]" {
		t.Errorf("empty listing body = %q, want []", strings.TrimSpace(body))
	}
}

func TestAPIListMessages(t *testing.T) {
	in := start(t)

	first := inject(t, in, &sms.Message{From: "+15005550006", To: "+15551234567", Body: "first"})
	second := injectMeta(t, in,
		&sms.Message{From: "+15005550006", To: "+15559999999", Body: strings.Repeat("a", 161)},
		map[string]any{"account_sid": "ACfake"})

	got := listMessages(t, in, "")
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}

	// Newest first, like every other listing in tommy.
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("listing order = %v, %v; want newest (%v) first", got[0].ID, got[1].ID, second.ID)
	}

	newest := got[0]
	if newest.Provider != "fake" || newest.Type != sms.EventType {
		t.Errorf("envelope = provider %q type %q, want fake / %s", newest.Provider, newest.Type, sms.EventType)
	}
	if newest.ReceivedAt.IsZero() {
		t.Error("envelope has no ReceivedAt")
	}
	if newest.Meta["account_sid"] != "ACfake" {
		t.Errorf("Meta = %v, want the provider metadata the event carried", newest.Meta)
	}
	if newest.Message == nil {
		t.Fatal("envelope carries no message")
	}
	if newest.Message.To != "+15559999999" {
		t.Errorf("To = %q", newest.Message.To)
	}
	if newest.Message.Status != sms.StatusQueued || newest.Message.Direction != sms.Outbound {
		t.Errorf("status/direction = %q/%q, want queued/outbound", newest.Message.Status, newest.Message.Direction)
	}
	want := sms.Segments{Count: 2, Encoding: sms.GSM7, Units: 161, Capacity: 153, Remaining: 145}
	if newest.Message.Segments != want {
		t.Errorf("Segments = %#v, want %#v", newest.Message.Segments, want)
	}
}

func TestAPIListFilters(t *testing.T) {
	in := start(t)

	inject(t, in, &sms.Message{From: "+15005550006", To: "+15551234567", Body: "plain ascii"})
	inject(t, in, &sms.Message{From: "+15005550007", To: "+15559999999", Body: "emoji \U0001F600", Status: sms.StatusDelivered})
	inject(t, in, &sms.Message{From: "+15551234567", To: "+15005550006", Body: "inbound reply", Direction: sms.Inbound})
	inject(t, in, &sms.Message{
		From:  "+15005550006",
		To:    "+15551234567",
		Body:  "with a picture",
		Media: []sms.Media{{ContentType: "image/png", URL: "https://example.com/cat.png"}},
	})

	tests := []struct {
		name      string
		query     string
		wantCount int
		wantBody  string
	}{
		{"no filter", "", 4, ""},
		{"by destination", "to=%2B15559999999", 1, "emoji \U0001F600"},
		{"by destination is case insensitive for sender ids", "to=%2B15551234567", 2, ""},
		{"by sender", "from=%2B15005550007", 1, "emoji \U0001F600"},
		{"by status", "status=delivered", 1, "emoji \U0001F600"},
		{"by status, uppercase", "status=DELIVERED", 1, "emoji \U0001F600"},
		{"by direction", "direction=inbound", 1, "inbound reply"},
		{"by encoding", "encoding=UCS-2", 1, "emoji \U0001F600"},
		{"by encoding, lowercase", "encoding=ucs-2", 1, "emoji \U0001F600"},
		{"gsm-7 only", "encoding=GSM-7", 3, ""},
		{"mms only", "mms=1", 1, "with a picture"},
		{"sms only", "mms=0", 3, ""},
		{"full text search", "search=picture", 1, "with a picture"},
		{"search matches a number", "search=15559999999", 1, "emoji \U0001F600"},
		{"by provider", "provider=fake", 4, ""},
		{"by an unknown provider", "provider=twilio", 0, ""},
		{"by type", "type=sms.message", 4, ""},
		{"limit", "limit=2", 2, ""},
		{"limit past the end", "limit=99", 4, ""},
		{"offset", "offset=3", 1, "plain ascii"},
		{"offset past the end", "offset=99", 0, ""},
		{"a filter and a limit together", "encoding=GSM-7&limit=1", 1, "with a picture"},
		{"nothing matches", "to=%2B10000000000", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := listMessages(t, in, tt.query)
			if len(got) != tt.wantCount {
				bodies := make([]string, 0, len(got))
				for _, m := range got {
					bodies = append(bodies, m.Message.Body)
				}
				t.Fatalf("?%s returned %d messages %q, want %d", tt.query, len(got), bodies, tt.wantCount)
			}
			if tt.wantBody != "" && got[0].Message.Body != tt.wantBody {
				t.Errorf("?%s first body = %q, want %q", tt.query, got[0].Message.Body, tt.wantBody)
			}
		})
	}

	t.Run("a bad since is a 400", func(t *testing.T) {
		status, body := in.GetBody(in.API("/sms/messages?since=yesterday"))
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
		if !strings.Contains(body, "error") {
			t.Errorf("body = %q, want a JSON error", body)
		}
	})

	t.Run("the plugin filter cannot be widened", func(t *testing.T) {
		if got := listMessages(t, in, "plugin=mail"); len(got) != 4 {
			t.Errorf("?plugin=mail returned %d, want the sms messages regardless", len(got))
		}
	})
}

func TestAPIListOnlyReturnsSMS(t *testing.T) {
	in := start(t)
	inject(t, in, &sms.Message{From: "+1500", To: "+1555", Body: "mine"})
	// Another plugin's event must not leak into the sms listing.
	if err := in.Store.Append(context.Background(), &event.Event{
		Plugin: "mail", Provider: "smtp", Type: "mail.message",
		Payload: map[string]any{"subject": "not an sms"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := listMessages(t, in, ""); len(got) != 1 {
		t.Fatalf("got %d messages, want only the sms one", len(got))
	}
}

func TestAPIGetMessage(t *testing.T) {
	in := start(t)
	ev := inject(t, in, &sms.Message{From: "+15005550006", To: "+15551234567", Body: "hello"})

	var got sms.MessageEnvelope
	if status := in.GetJSON(in.API("/sms/messages/"+string(ev.ID)), &got); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.ID != ev.ID || got.Message.Body != "hello" {
		t.Errorf("got %+v, want the injected message", got)
	}

	t.Run("an unknown id is a 404", func(t *testing.T) {
		status, body := in.GetBody(in.API("/sms/messages/nope"))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
		if !strings.Contains(body, "message not found") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("another plugin's event is a 404, not somebody else's payload", func(t *testing.T) {
		other := &event.Event{Plugin: "mail", Type: "mail.message", Payload: map[string]any{"subject": "x"}}
		if err := in.Store.Append(context.Background(), other); err != nil {
			t.Fatal(err)
		}
		status, _ := in.GetBody(in.API("/sms/messages/" + string(other.ID)))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
	})

	t.Run("an sms event with no message is a 404 that says why", func(t *testing.T) {
		broken := &event.Event{Plugin: sms.Name, Type: sms.EventType, Payload: "not a message"}
		if err := in.Store.Append(context.Background(), broken); err != nil {
			t.Fatal(err)
		}
		status, body := in.GetBody(in.API("/sms/messages/" + string(broken.ID)))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
		if !strings.Contains(body, "carries no sms message") {
			t.Errorf("body = %q", body)
		}
	})
}

func TestAPIMedia(t *testing.T) {
	in := start(t)
	ref := putBlob(t, in, "image/png", "cat.png", onePixelPNG)
	ev := inject(t, in, &sms.Message{
		From: "+15005550006",
		To:   "+15551234567",
		Body: "look at this",
		Media: []sms.Media{
			{ContentType: "image/png", Filename: "cat.png", Blob: &ref},
			{ContentType: "image/gif", URL: "https://example.com/party.gif"},
		},
	})

	envelope := listMessages(t, in, "")[0]
	if len(envelope.Media) != 2 {
		t.Fatalf("envelope has %d media, want 2", len(envelope.Media))
	}

	stored, remote := envelope.Media[0], envelope.Media[1]
	wantURL := "/api/v1/sms/messages/" + string(ev.ID) + "/media/0"
	if !stored.Stored || stored.URL != wantURL {
		t.Errorf("stored media = %+v, want a tommy URL %q", stored, wantURL)
	}
	if stored.Size != int64(len(onePixelPNG)) {
		t.Errorf("stored media size = %d, want %d", stored.Size, len(onePixelPNG))
	}
	if remote.Stored || remote.URL != "https://example.com/party.gif" {
		t.Errorf("remote media = %+v, want the provider's own URL", remote)
	}

	t.Run("the bytes come back with the recorded content type", func(t *testing.T) {
		resp := in.Get(in.API("/sms/messages/" + string(ev.ID) + "/media/0"))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("Content-Type = %q, want image/png", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="cat.png"`) {
			t.Errorf("Content-Disposition = %q", cd)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, onePixelPNG) {
			t.Errorf("got %d bytes, want the %d stored", len(body), len(onePixelPNG))
		}
	})

	t.Run("range requests work, so a browser can seek media", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, in.API("/sms/messages/"+string(ev.ID)+"/media/0"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-3")
		resp := in.Do(req)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, onePixelPNG[:4]) {
			t.Errorf("range body = %v, want the first four bytes", body)
		}
	})

	t.Run("media supplied as a url has no bytes here, and says so", func(t *testing.T) {
		status, body := in.GetBody(in.API("/sms/messages/" + string(ev.ID) + "/media/1"))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
		var out map[string]string
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		if out["url"] != "https://example.com/party.gif" {
			t.Errorf("response = %v, want the provider URL handed back", out)
		}
	})

	t.Run("out of range and non-numeric indexes are 404s", func(t *testing.T) {
		for _, idx := range []string{"2", "-1", "abc", ""} {
			status, _ := in.GetBody(in.API("/sms/messages/" + string(ev.ID) + "/media/" + idx))
			if status != http.StatusNotFound {
				t.Errorf("media/%s status = %d, want 404", idx, status)
			}
		}
	})

	t.Run("a blob that vanished is a 404 rather than a panic", func(t *testing.T) {
		gone := blob.Ref{ID: "does-not-exist", Size: 1, ContentType: "image/png"}
		orphan := inject(t, in, &sms.Message{
			To:    "+15551234567",
			Media: []sms.Media{{ContentType: "image/png", Blob: &gone}},
		})
		status, body := in.GetBody(in.API("/sms/messages/" + string(orphan.ID) + "/media/0"))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
		if !strings.Contains(body, "blob store") {
			t.Errorf("body = %q", body)
		}
	})
}

func TestAPIDeleteMessages(t *testing.T) {
	in := start(t)
	inject(t, in, &sms.Message{From: "+1500", To: "+1555", Body: "one"})
	inject(t, in, &sms.Message{From: "+1500", To: "+1555", Body: "two"})
	if err := in.Store.Append(context.Background(), &event.Event{
		Plugin: "mail", Type: "mail.message", Payload: map[string]any{"subject": "keep me"},
	}); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodDelete, in.API("/sms/messages"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if got := listMessages(t, in, ""); len(got) != 0 {
		t.Fatalf("got %d messages after the delete, want 0", len(got))
	}
	// Clearing sms must not clear another plugin's events.
	if n := len(in.Events(store.Query{})); n != 1 {
		t.Errorf("%d events left in the store, want just the mail one", n)
	}
}

// TestAPIEndToEnd drives the whole process the way a user would: a real HTTP
// request to the ingress, then the read-back API.
func TestAPIEndToEnd(t *testing.T) {
	in := start(t)

	status, body := in.PostJSON(in.Ingress("/fake-sms/v1/messages"), map[string]any{
		"from": "+15005550006",
		"to":   "+15551234567",
		"body": strings.Repeat("a", 200),
	})
	if status != http.StatusCreated {
		t.Fatalf("ingress status = %d, want 201: %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if created["num_segments"] != float64(2) {
		t.Errorf("num_segments = %v, want 2", created["num_segments"])
	}

	got := listMessages(t, in, "")
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].Message.Segments.Count != 2 || got[0].Message.Segments.Encoding != sms.GSM7 {
		t.Errorf("segments = %#v, want 2 GSM-7 segments", got[0].Message.Segments)
	}
	// The raw request is on the event, whole.
	var ev event.Event
	if s := in.GetJSON(in.API("/events/"+string(got[0].ID)), &ev); s != http.StatusOK {
		t.Fatalf("fetching the event: status %d", s)
	}
	if ev.Raw.Transport != "http" || ev.Raw.Method != "POST" {
		t.Errorf("Raw = %+v, want the untouched HTTP request", ev.Raw)
	}
}
