package mail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/mailtest"
)

// getJSON fetches a plugin API route and decodes it.
func getJSON[T any](t *testing.T, in *testutil.Instance, path string) (int, T) {
	t.Helper()
	var out T
	status := in.GetJSON(in.API(path), &out)
	return status, out
}

func do(t *testing.T, in *testutil.Instance, method, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return in.Do(req)
}

// seedInbox injects three messages with fixed timestamps so ordering and
// filtering assertions are deterministic.
func seedInbox(t *testing.T, in *testutil.Instance) (older, middle, newer *event.Event) {
	t.Helper()
	base := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)

	older = inject(t, in, &mail.Message{
		From:    mail.Address{Name: "Alice", Email: "alice@example.com"},
		To:      []mail.Address{{Email: "bob@example.com"}},
		Subject: "Welcome aboard",
		Text:    "Glad to have you.",
	}, mailtest.WithReceivedAt(base))

	middle = inject(t, in, sampleMessage(t, in),
		mailtest.WithProvider("sendgrid"),
		mailtest.WithMeta(map[string]any{"categories": []string{"billing"}}),
		mailtest.WithReceivedAt(base.Add(time.Minute)))

	newer = inject(t, in, &mail.Message{
		From:    mail.Address{Email: "noreply@example.com"},
		To:      []mail.Address{{Email: "carol@example.com"}},
		Subject: "Password reset",
		HTML:    "<p>Click <a href=\"https://example.com\">here</a>.</p>",
	}, mailtest.WithProvider("mailjet"), mailtest.WithReceivedAt(base.Add(2*time.Minute)))

	return older, middle, newer
}

func TestListMessages(t *testing.T) {
	in := start(t)
	older, middle, newer := seedInbox(t, in)

	tests := []struct {
		name  string
		query string
		want  []event.ID
	}{
		{"newest first", "", []event.ID{newer.ID, middle.ID, older.ID}},
		{"by provider", "?provider=mailjet", []event.ID{newer.ID}},
		{"by search", "?search=Invoice", []event.ID{middle.ID}},
		{"to matches every recipient", "?to=carol@example.com", []event.ID{newer.ID, middle.ID}},
		{"to matches a bcc", "?to=dan@example.com", []event.ID{middle.ID}},
		{"by sender", "?from=alice", []event.ID{middle.ID, older.ID}},
		{"by subject", "?subject=password", []event.ID{newer.ID}},
		{"with attachments", "?has_attachments=1", []event.ID{middle.ID}},
		{"without attachments", "?has_attachments=0", []event.ID{newer.ID, older.ID}},
		{"limit", "?limit=2", []event.ID{newer.ID, middle.ID}},
		{"offset", "?offset=2", []event.ID{older.ID}},
		{"limit and offset", "?limit=1&offset=1", []event.ID{middle.ID}},
		{"offset past the end", "?offset=99", nil},
		{"filters compose", "?has_attachments=0&from=alice", []event.ID{older.ID}},
		{"no match", "?to=nobody@example.com", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, views := getJSON[[]mail.MessageView](t, in, "/mail/messages"+tt.query)
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			var got []event.ID
			for _, v := range views {
				got = append(got, v.ID)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ids = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ids = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestListEncodesTheCanonicalMessage(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	status, views := getJSON[[]mail.MessageView](t, in, "/mail/messages?search=Invoice")
	if status != http.StatusOK || len(views) != 1 {
		t.Fatalf("status %d, %d results", status, len(views))
	}
	v := views[0]
	if v.Provider != "sendgrid" || v.Type != mail.TypeMessage || v.ID != middle.ID {
		t.Errorf("envelope = %+v", v)
	}
	if v.ReceivedAt.IsZero() {
		t.Error("received_at is missing")
	}
	if v.Meta["categories"] == nil {
		t.Errorf("provider metadata is missing from meta: %+v", v.Meta)
	}
	m := v.Message
	if m == nil {
		t.Fatal("the message is missing")
	}
	if m.From.Email != "alice@example.com" || m.From.Name != "Alice" {
		t.Errorf("from = %+v", m.From)
	}
	if len(m.To) != 1 || len(m.Cc) != 1 || len(m.Bcc) != 1 || len(m.ReplyTo) != 1 {
		t.Errorf("recipients = %+v", m)
	}
	if m.Text == "" || m.HTML == "" {
		t.Error("both body parts should survive the API")
	}
	if got := m.Headers.Get("X-Campaign"); got != "billing" {
		t.Errorf("headers = %+v", m.Headers)
	}
	if len(m.Attachments) != 2 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	if m.Attachments[0].Blob.ID == "" || m.Attachments[1].ContentID != "logo@tommy" {
		t.Errorf("attachments = %+v", m.Attachments)
	}

	base := "/api/v1/mail/messages/" + string(middle.ID)
	want := mail.MessageLinks{
		Self: base,
		HTML: base + "/html",
		Text: base + "/text",
		Raw:  base + "/raw",
		Attachments: []string{
			base + "/attachments/0",
			base + "/attachments/1",
		},
	}
	if v.Links.Self != want.Self || v.Links.HTML != want.HTML || v.Links.Text != want.Text || v.Links.Raw != want.Raw {
		t.Errorf("links = %+v, want %+v", v.Links, want)
	}
	if len(v.Links.Attachments) != 2 || v.Links.Attachments[1] != want.Attachments[1] {
		t.Errorf("attachment links = %v", v.Links.Attachments)
	}
}

func TestGetMessage(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	status, view := getJSON[mail.MessageView](t, in, "/mail/messages/"+string(middle.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if view.ID != middle.ID || view.Message.Subject != "Invoice 42" {
		t.Errorf("view = %+v", view)
	}

	// A message-shaped route must not serve an event that is not a message.
	foreign := &event.Event{
		Plugin: "sms", Provider: "twilio", Type: "sms.message",
		Summary: event.Summary{Title: "not mail"},
	}
	if err := in.Store.Append(context.Background(), foreign); err != nil {
		t.Fatalf("append: %v", err)
	}

	for _, path := range []string{
		"/mail/messages/does-not-exist",
		"/mail/messages/" + string(foreign.ID),
	} {
		status, body := in.GetBody(in.API(path))
		if status != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, status)
		}
		if !strings.Contains(body, `"error"`) {
			t.Errorf("GET %s: body = %q, want a JSON error", path, body)
		}
	}
}

func TestBodyRoutes(t *testing.T) {
	in := start(t)
	textOnly := inject(t, in, &mail.Message{
		From: mail.Address{Email: "a@example.com"},
		Text: "plain only",
	})
	both := inject(t, in, &mail.Message{
		From: mail.Address{Email: "a@example.com"},
		Text: "the text part",
		HTML: "<p>the <b>html</b> part</p>",
	})

	t.Run("html", func(t *testing.T) {
		resp := in.Get(in.API("/mail/messages/" + string(both.ID) + "/html"))
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("content type = %q", got)
		}
		if string(body) != "<p>the <b>html</b> part</p>" {
			t.Errorf("body = %q, want the html part verbatim", body)
		}
		// The body is untrusted content from the application under test.
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("content-security-policy = %q, want scripts blocked", csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("the html body must be served with nosniff")
		}
	})

	t.Run("text", func(t *testing.T) {
		resp := in.Get(in.API("/mail/messages/" + string(both.ID) + "/text"))
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != "the text part" {
			t.Fatalf("status %d, body %q", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("content type = %q", got)
		}
	})

	t.Run("raw", func(t *testing.T) {
		resp := in.Get(in.API("/mail/messages/" + string(both.ID) + "/raw"))
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if !strings.Contains(string(body), "the text part") {
			t.Errorf("raw body = %q, want the captured request", body)
		}
	})

	t.Run("raw download", func(t *testing.T) {
		resp := in.Get(in.API("/mail/messages/" + string(both.ID) + "/raw?download=1"))
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		cd := resp.Header.Get("Content-Disposition")
		if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, string(both.ID)) {
			t.Errorf("content-disposition = %q", cd)
		}
	})

	t.Run("missing parts are 404", func(t *testing.T) {
		for _, path := range []string{
			"/mail/messages/" + string(textOnly.ID) + "/html",
			"/mail/messages/does-not-exist/html",
			"/mail/messages/does-not-exist/text",
			"/mail/messages/does-not-exist/raw",
		} {
			if status, _ := in.GetBody(in.API(path)); status != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want 404", path, status)
			}
		}
	})
}

func TestAttachmentDownloadRoundTrip(t *testing.T) {
	in := start(t)
	ev := inject(t, in, sampleMessage(t, in))
	base := "/mail/messages/" + string(ev.ID) + "/attachments/"

	t.Run("bytes survive the blob store", func(t *testing.T) {
		resp := in.Get(in.API(base + "0"))
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if string(body) != "invoice,42\n" {
			t.Errorf("body = %q", body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/csv" {
			t.Errorf("content type = %q", got)
		}
		if got := resp.ContentLength; got != int64(len("invoice,42\n")) {
			t.Errorf("content length = %d", got)
		}
		kind, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if err != nil || kind != "attachment" || params["filename"] != "invoice.csv" {
			t.Errorf("content-disposition = %q (%v)", resp.Header.Get("Content-Disposition"), err)
		}
	})

	t.Run("an inline part is offered inline", func(t *testing.T) {
		resp := in.Get(in.API(base + "1"))
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		kind, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if err != nil || kind != "inline" || params["filename"] != "logo.png" {
			t.Errorf("content-disposition = %q (%v)", resp.Header.Get("Content-Disposition"), err)
		}
		if got := resp.Header.Get("Content-Type"); got != "image/png" {
			t.Errorf("content type = %q", got)
		}
	})

	t.Run("download forces a download", func(t *testing.T) {
		resp := in.Get(in.API(base + "1?download=1"))
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		if kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); kind != "attachment" {
			t.Errorf("content-disposition = %q", resp.Header.Get("Content-Disposition"))
		}
	})

	t.Run("range requests are supported", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, in.API(base+"0"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-6")
		resp := in.Do(req)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		if string(body) != "invoice" {
			t.Errorf("ranged body = %q", body)
		}
	})

	t.Run("bad indexes are 404", func(t *testing.T) {
		for _, path := range []string{base + "2", base + "-1", base + "abc", "/mail/messages/nope/attachments/0"} {
			if status, _ := in.GetBody(in.API(path)); status != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want 404", path, status)
			}
		}
	})
}

// A filename that is not plain ASCII must be encoded rather than mangled.
func TestAttachmentFilenameEncoding(t *testing.T) {
	in := start(t)
	m := &mail.Message{From: mail.Address{Email: "a@example.com"}, Subject: "receipt"}
	if _, err := m.AttachBytes(context.Background(), in.Blobs, mail.Attachment{
		Filename:    "счёт годовой.pdf",
		ContentType: "application/pdf",
	}, []byte("%PDF-1.4")); err != nil {
		t.Fatalf("attach: %v", err)
	}
	ev := inject(t, in, m)

	resp := in.Get(in.API("/mail/messages/" + string(ev.ID) + "/attachments/0"))
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	cd := resp.Header.Get("Content-Disposition")
	kind, params, err := mime.ParseMediaType(cd)
	if err != nil {
		t.Fatalf("parse %q: %v", cd, err)
	}
	if kind != "attachment" || params["filename"] != "счёт годовой.pdf" {
		t.Errorf("content-disposition = %q, decoded %q", cd, params["filename"])
	}
}

func TestClearMessages(t *testing.T) {
	in := start(t)
	ev := inject(t, in, sampleMessage(t, in))

	var before []mail.MessageView
	_ = in.GetJSON(in.API("/mail/messages"), &before)
	if len(before) != 1 {
		t.Fatalf("expected one message before clearing, got %d", len(before))
	}
	blobID := before[0].Message.Attachments[0].Blob.ID

	resp := do(t, in, http.MethodDelete, in.API("/mail/messages"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}

	var after []mail.MessageView
	_ = in.GetJSON(in.API("/mail/messages"), &after)
	if len(after) != 0 {
		t.Fatalf("messages survived the clear: %+v", after)
	}
	if status, _ := in.GetBody(in.API("/mail/messages/" + string(ev.ID))); status != http.StatusNotFound {
		t.Errorf("cleared message status = %d, want 404", status)
	}

	// Blobs deliberately outlive events: a download link that is already open
	// must keep working (docs/contracts.md, deviation 2).
	if _, err := in.Blobs.Stat(context.Background(), blobID); err != nil {
		t.Errorf("clearing events must not delete attachment blobs: %v", err)
	}
}

// One request fanning out into several messages is the shape both Mailjet and
// SendGrid have, so the plugin has to cope with it end to end.
func TestFanOutThroughTheIngress(t *testing.T) {
	in := start(t)

	payload := map[string]any{
		"messages": []map[string]any{
			{
				"from": "Alice <alice@example.com>", "to": []string{"bob@example.com"},
				"subject": "Fanned one", "text": "first",
			},
			{
				"from": "Alice <alice@example.com>", "to": []string{"carol@example.com"},
				"cc": []string{"dan@example.com"}, "subject": "Fanned two",
				"html":    "<p>second</p>",
				"headers": map[string]string{"X-Trace": "abc"},
				"attachments": []map[string]any{{
					"filename":     "note.txt",
					"content_type": "text/plain",
					"content":      base64.StdEncoding.EncodeToString([]byte("attached bytes")),
				}},
			},
		},
	}
	status, body := in.PostJSON(in.Ingress(mailtest.SendPath), payload)
	if status != http.StatusCreated {
		t.Fatalf("send status = %d: %s", status, body)
	}
	var sent struct {
		Messages []struct{ ID, Status string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode send response %q: %v", body, err)
	}
	if len(sent.Messages) != 2 {
		t.Fatalf("send response = %s, want two ids", body)
	}

	in.WaitForEvents(2, store.Query{Plugin: mail.PluginName}, 2*time.Second)

	var views []mail.MessageView
	if status := in.GetJSON(in.API("/mail/messages"), &views); status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if len(views) != 2 {
		t.Fatalf("one request should have produced two messages, got %d", len(views))
	}
	// Newest first: the second message of the batch leads.
	second, first := views[0], views[1]
	if first.Message.Subject != "Fanned one" || second.Message.Subject != "Fanned two" {
		t.Fatalf("subjects = %q, %q", first.Message.Subject, second.Message.Subject)
	}
	if second.Meta["fan_out_index"] != float64(1) {
		t.Errorf("fan-out index = %v, want it recorded in meta", second.Meta["fan_out_index"])
	}
	if got := second.Message.Headers.Get("X-Trace"); got != "abc" {
		t.Errorf("headers = %+v", second.Message.Headers)
	}
	if len(second.Message.Attachments) != 1 {
		t.Fatalf("attachments = %+v", second.Message.Attachments)
	}

	// The bytes went to the blob store, not into the event.
	resp := in.Get(in.API("/mail/messages/" + string(second.ID) + "/attachments/0"))
	defer func() { _ = resp.Body.Close() }()
	att, _ := io.ReadAll(resp.Body)
	if string(att) != "attached bytes" {
		t.Errorf("attachment = %q", att)
	}

	// Read-back serves from the store, so a client that sends then fetches sees
	// its own write.
	if status, _ := in.GetBody(in.API("/mail/messages/" + sent.Messages[0].ID)); status != http.StatusOK {
		t.Errorf("read-back status = %d", status)
	}
	if status, _ := in.GetBody(in.Ingress("/mailtest/v1/nope")); status != http.StatusNotFound {
		t.Errorf("unknown ingress path status = %d", status)
	}
}

func TestListRejectsABadQuery(t *testing.T) {
	in := start(t)
	status, body := in.GetBody(in.API("/mail/messages?limit=banana"))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(body, "limit") {
		t.Errorf("body = %q, want it to name the bad parameter", body)
	}
}

// The link to a message's own page rides on the read-back API, because
// /api/v1/mail/messages is what an application polling for "did my mail
// arrive" actually calls, and the answer to that question is usually "show me".
func TestMessagesCarryTheirPageURL(t *testing.T) {
	in := start(t)
	_, _, newer := seedInbox(t, in)

	want := in.UI("/events/" + string(newer.ID))

	status, views := getJSON[[]mail.MessageView](t, in, "/mail/messages")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(views) == 0 || views[0].URL != want {
		t.Errorf("listed url = %q, want %q", views[0].URL, want)
	}

	status, one := getJSON[mail.MessageView](t, in, "/mail/messages/"+string(newer.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if one.URL != want {
		t.Errorf("url = %q, want %q", one.URL, want)
	}
	// Absolute, because the sender is talking to the ingress on another port.
	if !strings.HasPrefix(one.URL, "http://") {
		t.Errorf("url = %q, want an absolute link", one.URL)
	}
	// And it is a page that exists.
	if status, _ := in.GetBody(one.URL); status != http.StatusOK {
		t.Errorf("the message page is broken: status %d", status)
	}
}
