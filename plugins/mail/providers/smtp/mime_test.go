package smtp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/plugins/mail"
)

type wantAttachment struct {
	filename    string
	contentType string
	contentID   string
	inline      bool
	content     string
}

type parseCase struct {
	name        string
	file        string
	from        string
	to          []string
	cc          []string
	replyTo     []string
	subject     string
	text        string
	html        string
	attachments []wantAttachment
	headers     map[string]string
	warnings    []string // substrings that must appear
	noWarnings  bool
}

// TestParseMessage is the heart of this provider's tests: every MIME shape it
// claims to handle, as a real message on disk, checked against the canonical
// model it must produce.
func TestParseMessage(t *testing.T) {
	cases := []parseCase{
		{
			name:       "plain text, no MIME structure at all",
			file:       "plain.eml",
			from:       "\"Alice\" <alice@example.com>",
			to:         []string{"bob@example.com"},
			subject:    "Just a plain note",
			text:       "Hello Bob,\n\nNothing fancy here: no MIME headers, no parts, just text.\n\n-- Alice\n",
			headers:    map[string]string{"Message-ID": "<plain-1@example.com>", "Date": "Tue, 12 Mar 2024 09:15:00 +0000"},
			noWarnings: true,
		},
		{
			name:    "multipart/alternative, text and quoted-printable html",
			file:    "alternative.eml",
			from:    "\"Alice Sender\" <alice@example.com>",
			to:      []string{"bob@example.com", "carol@example.com"},
			cc:      []string{"dave@example.com"},
			replyTo: []string{"replies@example.com", "alice@example.com"},
			subject: "Your receipt",
			text:    "Total: 42 EUR\n",
			// The quoted-printable soft break and =C2=A0 both had to decode.
			html:       "<html><body><p>Total: <b>42 EUR</b> — thank you!</p></body></html>\n",
			noWarnings: true,
		},
		{
			name:    "multipart/mixed wrapping multipart/alternative, two attachments",
			file:    "mixed_nested.eml",
			from:    "\"Alice\" <alice@example.com>",
			to:      []string{"bob@example.com"},
			subject: "Invoice attached",
			text:    "Invoice attached.\n",
			html:    "<p>Invoice attached.</p>\n",
			attachments: []wantAttachment{
				{filename: "invoice.csv", contentType: "text/csv; charset=utf-8", content: "id,total\r\n1,42\r\n"},
				{filename: "invoice.pdf", contentType: "application/pdf", content: "%PDF-1.4 fake pdf bytes\r\n%%EOF\r\n"},
			},
			noWarnings: true,
		},
		{
			name:    "multipart/related with a cid image around an alternative",
			file:    "related_inline.eml",
			from:    "\"Newsletter\" <news@example.com>",
			to:      []string{"bob@example.com"},
			subject: "Look at this logo",
			text:    "Our logo is above.\n",
			html:    "<p>Our logo: <img src=\"cid:logo@example.com\"></p>\n",
			attachments: []wantAttachment{
				{contentType: "image/png", contentID: "logo@example.com", inline: true,
					content: "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"},
			},
			noWarnings: true,
		},
		{
			name:    "RFC 2047 encoded words: several in a row, mixed charsets, folded",
			file:    "encoded_words.eml",
			from:    "=?utf-8?q?Alice_=C3=85ngstr=C3=B6m?= <alice@example.com>",
			to:      []string{"bob@example.com", "carol@example.com"},
			subject: "Hello world plain café and a folded tail",
			text:    "Encoded-word headers above.\n",
			headers: map[string]string{"X-Custom-Header": "kept as-is"},
			// The header copy is decoded too, so the UI never shows =?UTF-8?B?...
			noWarnings: true,
		},
		{
			name:       "a charset that is not UTF-8",
			file:       "charset_latin1.eml",
			from:       "\"Hans\" <hans@example.de>",
			to:         []string{"bob@example.com"},
			subject:    "Grüße",
			text:       "Zurück zum Café, natürlich.\nGrüße aus München.\n",
			noWarnings: true,
		},
		{
			name:     "an unknown charset degrades instead of corrupting",
			file:     "charset_unknown.eml",
			from:     "someone@example.com",
			to:       []string{"bob@example.com"},
			subject:  "Unknown charset",
			text:     "plain ascii survives anything\n",
			warnings: []string{"unknown charset"},
		},
		{
			name:    "an RFC 2231 filename split across continuations",
			file:    "rfc2231_filename.eml",
			from:    "\"Alice\" <alice@example.com>",
			to:      []string{"bob@example.com"},
			subject: "Rates",
			text:    "See attached.\n",
			attachments: []wantAttachment{
				{filename: "€ rates 2024.csv", contentType: "application/octet-stream", content: "a,b\r\n1,2\r\n"},
			},
			noWarnings: true,
		},
		{
			name:    "quoted-printable, base64 and 8bit in one message",
			file:    "transfer_encodings.eml",
			from:    "\"Alice\" <alice@example.com>",
			to:      []string{"bob@example.com"},
			subject: "Encodings",
			text:    "Soft spaces and a soft line break here and it continues.\n",
			html:    "<p>base64 html</p>",
			attachments: []wantAttachment{
				{filename: "payload.json", contentType: "application/json", content: "{\"ok\": true}\r\n"},
			},
			noWarnings: true,
		},
		{
			name:    "a malformed message fails safely and keeps what it can",
			file:    "malformed.eml",
			from:    "not a real address",
			to:      []string{"bob@example.com", "carol@example.com"},
			subject: "=?UTF-8?B?!!!not-base64!!!?=",
			text:    "the boundary below never closes\n",
			attachments: []wantAttachment{
				{filename: "broken.bin", contentType: "application/octet-stream", content: ""},
			},
			warnings: []string{"did not parse", "unexpected EOF", "base64 body could not be fully decoded"},
		},
		{
			name:     "no header/body separator at all",
			file:     "no_separator.eml",
			text:     "this is not a message, it is just some bytes\n",
			warnings: []string{"no header/body separator"},
		},
		{
			name:       "headers but no addresses",
			file:       "headerless_body.eml",
			subject:    "Only a subject",
			text:       "body\n",
			noWarnings: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := readFixture(t, tc.file)
			blobs := blobmem.New(8 << 20)
			msg, warns, err := ParseMessage(context.Background(), raw, blobs)
			if err != nil {
				t.Fatalf("ParseMessage: %v", err)
			}

			if got := msg.From.String(); got != tc.from {
				t.Errorf("From = %q, want %q", got, tc.from)
			}
			checkAddrs(t, "To", msg.To, tc.to)
			checkAddrs(t, "Cc", msg.Cc, tc.cc)
			checkAddrs(t, "Reply-To", msg.ReplyTo, tc.replyTo)
			if msg.Subject != tc.subject {
				t.Errorf("Subject = %q, want %q", msg.Subject, tc.subject)
			}
			if msg.Text != tc.text {
				t.Errorf("Text = %q, want %q", msg.Text, tc.text)
			}
			if msg.HTML != tc.html {
				t.Errorf("HTML = %q, want %q", msg.HTML, tc.html)
			}
			for k, v := range tc.headers {
				if got := msg.Headers.Get(k); got != v {
					t.Errorf("Headers[%s] = %q, want %q", k, got, v)
				}
			}

			if len(msg.Attachments) != len(tc.attachments) {
				t.Fatalf("got %d attachments, want %d: %+v", len(msg.Attachments), len(tc.attachments), msg.Attachments)
			}
			for i, want := range tc.attachments {
				got := msg.Attachments[i]
				if got.Filename != want.filename {
					t.Errorf("attachment %d: Filename = %q, want %q", i, got.Filename, want.filename)
				}
				if got.ContentType != want.contentType {
					t.Errorf("attachment %d: ContentType = %q, want %q", i, got.ContentType, want.contentType)
				}
				if got.ContentID != want.contentID {
					t.Errorf("attachment %d: ContentID = %q, want %q", i, got.ContentID, want.contentID)
				}
				if got.Inline != want.inline {
					t.Errorf("attachment %d: Inline = %v, want %v", i, got.Inline, want.inline)
				}
				if got.Size != int64(len(want.content)) {
					t.Errorf("attachment %d: Size = %d, want %d", i, got.Size, len(want.content))
				}
				// The bytes must be in the blob store, never in the event.
				if body := readBlob(t, blobs, got.Blob); body != want.content {
					t.Errorf("attachment %d: blob content = %q, want %q", i, body, want.content)
				}
			}

			if tc.noWarnings && len(warns) > 0 {
				t.Errorf("unexpected parse warnings: %v", warns)
			}
			for _, want := range tc.warnings {
				if !containsSubstring(warns, want) {
					t.Errorf("warnings %v do not mention %q", warns, want)
				}
			}
		})
	}
}

// TestParseMessageInlineImageIsFoundByCID proves the round trip the mail
// plugin's HTML view depends on: a cid: URL in the body resolves to a stored
// attachment.
func TestParseMessageInlineImageIsFoundByCID(t *testing.T) {
	raw := readFixture(t, "related_inline.eml")
	msg, _, err := ParseMessage(context.Background(), raw, blobmem.New(1<<20))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	att, ok := msg.AttachmentByContentID("cid:logo@example.com")
	if !ok {
		t.Fatalf("no attachment for cid:logo@example.com; have %+v", msg.Attachments)
	}
	if !att.Inline || att.ContentType != "image/png" {
		t.Errorf("attachment = %+v, want an inline image/png", att)
	}
}

func checkAddrs(t *testing.T, what string, got []mail.Address, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, mail.Emails(got), want)
		return
	}
	for i := range want {
		if got[i].Email != want[i] {
			t.Errorf("%s[%d] = %q, want %q", what, i, got[i].Email, want[i])
		}
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func readBlob(t *testing.T, blobs blob.BlobStore, ref blob.Ref) string {
	t.Helper()
	rc, _, err := blobs.Open(context.Background(), ref.ID)
	if err != nil {
		t.Fatalf("open blob %s: %v", ref.ID, err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob %s: %v", ref.ID, err)
	}
	return string(body)
}
