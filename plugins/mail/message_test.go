package mail_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/mail"
)

func TestAddressString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr mail.Address
		want string
	}{
		{"bare", mail.Address{Email: "bob@example.com"}, "bob@example.com"},
		{"named", mail.Address{Name: "Bob", Email: "bob@example.com"}, `"Bob" <bob@example.com>`},
		{"name needing quotes", mail.Address{Name: "Bob, Jr.", Email: "bob@example.com"}, `"Bob, Jr." <bob@example.com>`},
		{"unicode name", mail.Address{Name: "Zoë", Email: "z@example.com"}, "=?utf-8?q?Zo=C3=AB?= <z@example.com>"},
		{"empty", mail.Address{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.addr.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    mail.Address
		wantErr bool
	}{
		{"bare", "bob@example.com", mail.Address{Email: "bob@example.com"}, false},
		{"named", "Bob <bob@example.com>", mail.Address{Name: "Bob", Email: "bob@example.com"}, false},
		{"quoted", `"Bob, Jr." <bob@example.com>`, mail.Address{Name: "Bob, Jr.", Email: "bob@example.com"}, false},
		{"encoded word", "=?utf-8?q?Zo=C3=AB?= <z@example.com>", mail.Address{Name: "Zoë", Email: "z@example.com"}, false},
		{"padded", "  bob@example.com  ", mail.Address{Email: "bob@example.com"}, false},
		{"garbage", "not an address", mail.Address{}, true},
		{"empty", "", mail.Address{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mail.ParseAddress(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseAddress(%q) = %+v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAddress(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseAddress(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseAddressList(t *testing.T) {
	t.Parallel()
	got, err := mail.ParseAddressList(`Bob <bob@example.com>, carol@example.com`)
	if err != nil {
		t.Fatalf("ParseAddressList: %v", err)
	}
	want := []mail.Address{{Name: "Bob", Email: "bob@example.com"}, {Email: "carol@example.com"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ParseAddressList = %+v, want %+v", got, want)
	}
	if list, err := mail.ParseAddressList("   "); err != nil || list != nil {
		t.Errorf("empty list = %+v, %v; want nil, nil", list, err)
	}
	if _, err := mail.ParseAddressList("nope"); err == nil {
		t.Error("ParseAddressList(\"nope\") should fail")
	}
}

func TestEmailsAndFormatAddressList(t *testing.T) {
	t.Parallel()
	list := []mail.Address{{Name: "Bob", Email: "bob@example.com"}, {Email: "carol@example.com"}, {}}
	emails := mail.Emails(list)
	if len(emails) != 2 || emails[0] != "bob@example.com" || emails[1] != "carol@example.com" {
		t.Errorf("Emails = %v", emails)
	}
	if got, want := mail.FormatAddressList(list), `"Bob" <bob@example.com>, carol@example.com`; got != want {
		t.Errorf("FormatAddressList = %q, want %q", got, want)
	}
	if mail.Emails(nil) != nil {
		t.Error("Emails(nil) should be nil")
	}
}

func TestHeaders(t *testing.T) {
	t.Parallel()
	var h mail.Headers // the zero value must be usable: providers build these up
	h.Set("X-Custom", "one")
	h.Add("Received", "from a")
	h.Add("received", "from b")

	if got := h.Get("x-custom"); got != "one" {
		t.Errorf("Get is not case-insensitive: %q", got)
	}
	if got := h.Values("RECEIVED"); len(got) != 2 || got[0] != "from a" || got[1] != "from b" {
		t.Errorf("Values = %v, want both Received values under one key", got)
	}
	if got := h.Get("missing"); got != "" {
		t.Errorf("Get(missing) = %q", got)
	}

	// Set replaces whatever case the name was first stored under.
	h.Set("RECEIVED", "only")
	if got := h.Values("received"); len(got) != 1 || got[0] != "only" {
		t.Errorf("after Set, Values = %v", got)
	}
	if keys := h.Keys(); len(keys) != 2 || keys[0] != "RECEIVED" || keys[1] != "X-Custom" {
		t.Errorf("Keys = %v, want them sorted", keys)
	}

	clone := h.Clone()
	clone.Add("X-Custom", "two")
	if len(h.Values("X-Custom")) != 1 {
		t.Error("Clone must not share slices with the original")
	}
	if mail.Headers(nil).Clone() != nil {
		t.Error("Clone of nil should be nil")
	}
}

func TestMessageSnippet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  mail.Message
		want string
	}{
		{"prefers text", mail.Message{Text: "plain wins", HTML: "<p>html loses</p>"}, "plain wins"},
		{"collapses whitespace", mail.Message{Text: "  a\n\n\tb  "}, "a b"},
		{"strips tags", mail.Message{HTML: "<p>Hello <b>world</b></p>"}, "Hello world"},
		{"drops script and style", mail.Message{HTML: "<style>p{color:red}</style><p>visible</p><script>alert(1)</script>"}, "visible"},
		{"unescapes entities", mail.Message{HTML: "<p>a &amp; b &lt;c&gt;</p>"}, "a & b <c>"},
		{"breaks on br", mail.Message{HTML: "one<br>two"}, "one two"},
		{"empty", mail.Message{}, ""},
		{"unterminated tag", mail.Message{HTML: "before <b"}, "before"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.msg.Snippet(); got != tt.want {
				t.Errorf("Snippet() = %q, want %q", got, tt.want)
			}
		})
	}

	long := mail.Message{Text: strings.Repeat("x", mail.SnippetLimit*2)}
	got := long.Snippet()
	if n := len([]rune(got)); n != mail.SnippetLimit {
		t.Errorf("long snippet is %d runes, want %d", n, mail.SnippetLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated snippet should end in an ellipsis: %q", got)
	}
}

func TestMessageSummary(t *testing.T) {
	t.Parallel()
	m := &mail.Message{
		From:    mail.Address{Name: "Alice", Email: "alice@example.com"},
		To:      []mail.Address{{Email: "bob@example.com"}},
		Cc:      []mail.Address{{Email: "carol@example.com"}},
		Bcc:     []mail.Address{{Email: "dan@example.com"}},
		Subject: "Invoice 42",
		Text:    "Please pay.",
	}
	s := m.Summary()
	if s.From != `"Alice" <alice@example.com>` {
		t.Errorf("Summary.From = %q", s.From)
	}
	// Cc and Bcc belong in To: a Message is one delivered message, and
	// searching for a bcc'd address has to find it.
	want := []string{"bob@example.com", "carol@example.com", "dan@example.com"}
	if len(s.To) != len(want) {
		t.Fatalf("Summary.To = %v, want %v", s.To, want)
	}
	for i := range want {
		if s.To[i] != want[i] {
			t.Fatalf("Summary.To = %v, want %v", s.To, want)
		}
	}
	if s.Title != "Invoice 42" || s.Snippet != "Please pay." {
		t.Errorf("Summary = %+v", s)
	}
}

func TestRecipients(t *testing.T) {
	t.Parallel()
	m := &mail.Message{
		To:  []mail.Address{{Email: "a@example.com"}},
		Cc:  []mail.Address{{Email: "b@example.com"}},
		Bcc: []mail.Address{{Email: "c@example.com"}},
	}
	if got := m.RecipientEmails(); strings.Join(got, ",") != "a@example.com,b@example.com,c@example.com" {
		t.Errorf("RecipientEmails = %v", got)
	}
	if m.HasAttachments() {
		t.Error("HasAttachments should be false")
	}
}

func TestTrimContentID(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"<logo@tommy>", "logo@tommy"},
		{"cid:logo", "logo"},
		{"<cid:logo>", "logo"},
		{"  logo  ", "logo"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := mail.TrimContentID(tt.in); got != tt.want {
			t.Errorf("TrimContentID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAttachRoundTripsThroughTheBlobStore(t *testing.T) {
	t.Parallel()
	blobs := blobmem.New(1 << 20)
	m := &mail.Message{}
	data := []byte("invoice,42\n")

	got, err := m.AttachBytes(context.Background(), blobs, mail.Attachment{
		Filename:    "invoice.csv",
		ContentType: "text/csv",
	}, data)
	if err != nil {
		t.Fatalf("AttachBytes: %v", err)
	}
	if got.Size != int64(len(data)) || got.Blob.Size != int64(len(data)) {
		t.Errorf("size = %d / %d, want %d", got.Size, got.Blob.Size, len(data))
	}
	if got.Blob.ID == "" {
		t.Fatal("the attachment carries no blob id")
	}
	if got.Disposition() != "attachment" {
		t.Errorf("Disposition = %q", got.Disposition())
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Blob.ID != got.Blob.ID {
		t.Fatalf("Attach did not append to the message: %+v", m.Attachments)
	}

	rc, ref, err := blobs.Open(context.Background(), got.Blob.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	stored, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(stored) != string(data) {
		t.Errorf("blob bytes = %q, want %q", stored, data)
	}
	if ref.Filename != "invoice.csv" || ref.ContentType != "text/csv" {
		t.Errorf("blob ref = %+v", ref)
	}
}

func TestAttachInlineNormalisesContentID(t *testing.T) {
	t.Parallel()
	blobs := blobmem.New(1 << 20)
	m := &mail.Message{HTML: `<img src="cid:logo@tommy">`}
	got, err := m.AttachBytes(context.Background(), blobs, mail.Attachment{
		Filename:  "logo.png",
		Inline:    true,
		ContentID: "<logo@tommy>",
	}, []byte{0x89, 'P', 'N', 'G'})
	if err != nil {
		t.Fatalf("AttachBytes: %v", err)
	}
	if got.ContentID != "logo@tommy" {
		t.Errorf("ContentID = %q, want the angle brackets stripped", got.ContentID)
	}
	if got.ContentType != "application/octet-stream" {
		t.Errorf("ContentType = %q, want the octet-stream default", got.ContentType)
	}
	if got.Disposition() != "inline" {
		t.Errorf("Disposition = %q", got.Disposition())
	}
	found, ok := m.AttachmentByContentID("cid:LOGO@tommy")
	if !ok || found.Filename != "logo.png" {
		t.Errorf("AttachmentByContentID = %+v, %v", found, ok)
	}
	if _, ok := m.AttachmentByContentID(""); ok {
		t.Error("an empty cid must not match")
	}
}

func TestAttachFailures(t *testing.T) {
	t.Parallel()
	m := &mail.Message{}
	if _, err := m.AttachBytes(context.Background(), nil, mail.Attachment{}, []byte("x")); err == nil {
		t.Error("attaching without a blob store must fail")
	}
	if len(m.Attachments) != 0 {
		t.Error("a failed attach must not append to the message")
	}

	tiny := blobmem.New(4)
	if _, err := m.AttachBytes(context.Background(), tiny, mail.Attachment{Filename: "big.bin"}, []byte("more than four bytes")); !errors.Is(err, blob.ErrCapacityExceeded) {
		t.Errorf("over-capacity attach error = %v, want ErrCapacityExceeded", err)
	}
	if len(m.Attachments) != 0 {
		t.Error("a failed attach must not append to the message")
	}
}

func TestAttachmentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		att  mail.Attachment
		want string
	}{
		{mail.Attachment{Filename: "a.txt"}, "a.txt"},
		{mail.Attachment{ContentID: "logo"}, "logo"},
		{mail.Attachment{}, "attachment"},
	}
	for _, tt := range tests {
		if got := tt.att.Name(); got != tt.want {
			t.Errorf("Name() = %q, want %q", got, tt.want)
		}
	}
}

// Bytes never go inline: an attachment carries a blob reference, and the
// encoded message must not contain the attachment's contents.
func TestMessageJSONNeverCarriesBytes(t *testing.T) {
	t.Parallel()
	blobs := blobmem.New(1 << 20)
	m := &mail.Message{
		From:    mail.Address{Name: "Alice", Email: "alice@example.com"},
		To:      []mail.Address{{Email: "bob@example.com"}},
		Subject: "Report",
		Text:    "see attached",
	}
	secret := "SUPER-SECRET-ATTACHMENT-BYTES"
	if _, err := m.AttachBytes(context.Background(), blobs, mail.Attachment{Filename: "r.txt", ContentType: "text/plain"}, []byte(secret)); err != nil {
		t.Fatalf("AttachBytes: %v", err)
	}

	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("attachment bytes leaked into the message JSON: %s", encoded)
	}

	var back mail.Message
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Subject != m.Subject || back.From != m.From || len(back.To) != 1 {
		t.Errorf("round trip lost envelope fields: %+v", back)
	}
	if len(back.Attachments) != 1 || back.Attachments[0].Blob.ID != m.Attachments[0].Blob.ID {
		t.Errorf("round trip lost the blob reference: %+v", back.Attachments)
	}
}

func TestNewEvent(t *testing.T) {
	t.Parallel()
	m := &mail.Message{From: mail.Address{Email: "a@example.com"}, Subject: "Hi"}
	ev := mail.NewEvent("sendgrid", m)
	if ev.Plugin != mail.PluginName || ev.Type != mail.TypeMessage || ev.Provider != "sendgrid" {
		t.Errorf("event envelope = %+v", ev)
	}
	if ev.Payload != any(m) {
		t.Error("Payload must be the message itself, not a copy")
	}
	if ev.Summary.Title != "Hi" {
		t.Errorf("Summary was not filled in: %+v", ev.Summary)
	}
	if ev.ID != "" || !ev.ReceivedAt.IsZero() {
		t.Error("NewEvent must leave stamping to Deps.Append")
	}
}

func TestMessageOf(t *testing.T) {
	t.Parallel()
	m := &mail.Message{Subject: "Hi"}
	tests := []struct {
		name    string
		payload any
		wantOK  bool
		want    string
	}{
		{"pointer", m, true, "Hi"},
		{"value", *m, true, "Hi"},
		{"decoded json", map[string]any{"subject": "Hi", "from": map[string]any{"email": "a@example.com"}}, true, "Hi"},
		{"nil", nil, false, ""},
		{"unrelated", make(chan int), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &event.Event{Plugin: mail.PluginName, Type: mail.TypeMessage, Payload: tt.payload}
			got, ok := mail.MessageOf(e)
			if ok != tt.wantOK {
				t.Fatalf("MessageOf ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.Subject != tt.want {
				t.Errorf("MessageOf subject = %q, want %q", got.Subject, tt.want)
			}
		})
	}
	if _, ok := mail.MessageOf(nil); ok {
		t.Error("MessageOf(nil) must not report a message")
	}
}
