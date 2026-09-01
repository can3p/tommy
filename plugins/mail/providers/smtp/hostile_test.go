package smtp

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	blobmem "github.com/can3p/tommy/core/blob/memory"
)

// TestParseMessageEncodedWordDisplayNames pins the decoded display names, which
// the address String() round trip in the table above re-encodes.
func TestParseMessageEncodedWordDisplayNames(t *testing.T) {
	msg, warns, err := ParseMessage(context.Background(), readFixture(t, "encoded_words.eml"), blobmem.New(1<<20))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if msg.From.Name != "Alice Ångström" {
		t.Errorf("From.Name = %q, want %q", msg.From.Name, "Alice Ångström")
	}
	if len(msg.To) != 2 || msg.To[0].Name != "Bob Büchner" || msg.To[1].Name != "Plain Name" {
		t.Errorf("To = %+v, want the decoded display names", msg.To)
	}
	// The header copy is decoded too, so nothing downstream has to know about
	// RFC 2047 - the wire form stays available in event.Raw.
	if got := msg.Headers.Get("Subject"); !strings.HasPrefix(got, "Hello world") {
		t.Errorf("Headers[Subject] = %q, want it decoded", got)
	}
}

// TestParseMessageHostile feeds the parser input designed to break it. None of
// these may panic, hang, or allocate without bound; every one must come back
// with a message and a warning.
func TestParseMessageHostile(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string // a warning substring, "" when none is required
	}{
		{
			name: "empty input",
			raw:  nil,
		},
		{
			name: "only a blank line",
			raw:  []byte("\r\n"),
		},
		{
			name: "header block that never ends",
			raw:  []byte("Subject: forever\r\nX-A: 1\r\nX-B: 2\r\n"),
		},
		{
			name: "a continuation line with nothing to continue",
			raw:  []byte("   orphaned\r\nSubject: ok\r\n\r\nbody\r\n"),
		},
		{
			name: "a header with no colon",
			raw:  []byte("Subject: fine\r\nthis is not a header\r\n\r\nbody\r\n"),
		},
		{
			name: "multipart with no boundary parameter",
			raw:  []byte("Content-Type: multipart/mixed\r\n\r\nnot really multipart\r\n"),
			want: "without a boundary parameter",
		},
		{
			name: "boundary that matches nothing",
			raw:  []byte("Content-Type: multipart/mixed; boundary=\"nope\"\r\n\r\nno parts here\r\n"),
			want: "multipart",
		},
		{
			name: "an unknown transfer encoding",
			raw:  []byte("Content-Transfer-Encoding: x-uuencode\r\n\r\nbegin 644 x\r\n"),
			want: "unknown Content-Transfer-Encoding",
		},
		{
			name: "nesting far deeper than any real message",
			raw:  deepNesting(maxDepth + 5),
			want: "nested deeper",
		},
		{
			name: "more parts than the limit",
			raw:  manyParts(maxParts + 50),
			want: "more than",
		},
		{
			name: "a header made of one enormous encoded word",
			raw:  []byte("Subject: =?UTF-8?B?" + strings.Repeat("QQ==", 5000) + "?=\r\n\r\nbody\r\n"),
		},
		{
			name: "a quoted-printable body full of broken escapes",
			raw:  []byte("Content-Transfer-Encoding: quoted-printable\r\n\r\n=ZZ=" + strings.Repeat("=", 200) + "\r\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blobs := blobmem.New(1 << 20)
			msg, warns, err := ParseMessage(context.Background(), tc.raw, blobs)
			if err != nil {
				t.Fatalf("ParseMessage returned an error on hostile input: %v", err)
			}
			if msg == nil {
				t.Fatal("ParseMessage returned no message")
			}
			if tc.want != "" && !containsSubstring(warns, tc.want) {
				t.Errorf("warnings %v do not mention %q", warns, tc.want)
			}
			if len(warns) > maxWarnings {
				t.Errorf("got %d warnings, more than the %d cap", len(warns), maxWarnings)
			}
		})
	}
}

// TestParseMessageBudget proves a small message cannot be made to allocate an
// unbounded amount by nesting the same bytes over and over.
func TestParseMessageBudget(t *testing.T) {
	raw := deepNesting(maxDepth - 1)
	msg, warns, err := ParseMessage(context.Background(), raw, blobmem.New(1<<20))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if msg.Text == "" && len(msg.Attachments) == 0 {
		t.Errorf("a legal deeply nested message lost its body: %+v, warns %v", msg, warns)
	}
}

// TestParseMessageBlobFull proves a full blob store costs the attachment, not
// the message: over-capacity is reported as a warning, never a panic.
func TestParseMessageBlobFull(t *testing.T) {
	// Room for the first attachment only.
	blobs := blobmem.New(20)
	raw := readFixture(t, "mixed_nested.eml")
	msg, warns, err := ParseMessage(context.Background(), raw, blobs)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if msg.Text == "" {
		t.Error("the message body was lost when the blob store filled up")
	}
	if len(msg.Attachments) != 1 {
		t.Errorf("got %d attachments, want the one that fit: %+v", len(msg.Attachments), msg.Attachments)
	}
	if !containsSubstring(warns, "did not fit in the blob store") {
		t.Errorf("warnings %v do not report the dropped attachment", warns)
	}
}

// deepNesting builds a message of n nested multipart/mixed parts with a text
// body at the bottom.
func deepNesting(n int) []byte {
	var buf bytes.Buffer
	buf.WriteString("Subject: nested\r\nMIME-Version: 1.0\r\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
	}
	buf.WriteString("Content-Type: text/plain\r\n\r\nbottom\r\n")
	for i := n - 1; i >= 0; i-- {
		fmt.Fprintf(&buf, "--b%d--\r\n", i)
	}
	return buf.Bytes()
}

// manyParts builds one multipart with n trivial parts.
func manyParts(n int) []byte {
	var buf bytes.Buffer
	buf.WriteString("Subject: many\r\nMIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&buf, "--b\r\nContent-Type: text/plain\r\n\r\npart %d\r\n", i)
	}
	buf.WriteString("--b--\r\n")
	return buf.Bytes()
}
