//go:build integration

// SMTP integration test uses net/smtp - the standard library is the
// "official client" for a protocol rather than a vendor HTTP API - to
// deliver a real MIME multipart message with an attachment, and checks
// tommy's SMTP listener parsed it faithfully: the text part, and the
// attachment's bytes round-tripped through the blob store unchanged.
package integration

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"testing"
	"time"

	"github.com/can3p/tommy/core/store"
	tommymail "github.com/can3p/tommy/plugins/mail"
	smtpprovider "github.com/can3p/tommy/plugins/mail/providers/smtp"
)

// buildMIMEMessage assembles a multipart/mixed message with a text part and
// one base64 attachment part, using mime/multipart rather than hand-rolled
// boundaries, so the bytes net/smtp.SendMail delivers are exactly what any
// real MIME-generating library would produce.
func buildMIMEMessage(from, to, subject, text, attachmentName, attachmentType string, attachmentData []byte) []byte {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textPart, _ := w.CreatePart(textHeader)
	_, _ = textPart.Write([]byte(text))

	attHeader := textproto.MIMEHeader{}
	attHeader.Set("Content-Type", attachmentType)
	attHeader.Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, attachmentName))
	attHeader.Set("Content-Transfer-Encoding", "base64")
	attPart, _ := w.CreatePart(attHeader)
	enc := base64.NewEncoder(base64.StdEncoding, attPart)
	_, _ = enc.Write(attachmentData)
	_ = enc.Close()

	_ = w.Close()

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\n", from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	fmt.Fprintf(&msg, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&msg, "Content-Type: multipart/mixed; boundary=%q\r\n", w.Boundary())
	fmt.Fprintf(&msg, "\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes()
}

// TestSMTPClientSendsMIMEWithAttachment delivers a real MIME message with an
// attachment over plain SMTP - the path most application frameworks take in
// development - and checks tommy's listener parsed both parts correctly.
func TestSMTPClientSendsMIMEWithAttachment(t *testing.T) {
	inst := startTommy(t)
	addr := waitForSMTPAddr(t, inst, 3*time.Second)

	const (
		from           = "alice@example.com"
		to             = "bob@example.com"
		subject        = "Hello from net/smtp"
		text           = "It works, with an attachment."
		attachmentName = "receipt.txt"
		attachmentType = "text/plain"
	)
	attachmentBody := []byte("smtp attachment body\n")

	msg := buildMIMEMessage(from, to, subject, text, attachmentName, attachmentType, attachmentBody)

	if err := smtp.SendMail(addr, nil, from, []string{to}, msg); err != nil {
		t.Fatalf("smtp.SendMail: %v", err)
	}

	inst.WaitForEvents(1, store.Query{Plugin: tommymail.PluginName, Provider: smtpprovider.ProviderName}, 3*time.Second)
	got := findMailEvent(t, inst, smtpprovider.ProviderName, subject, 3*time.Second)

	if got.From.Email != from {
		t.Errorf("From = %q, want %q", got.From.Email, from)
	}
	if len(got.To) != 1 || got.To[0].Email != to {
		t.Errorf("To = %+v, want [%s]", got.To, to)
	}
	if got.Text != text {
		t.Errorf("Text = %q, want %q", got.Text, text)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(got.Attachments))
	}

	att := got.Attachments[0]
	if att.Filename != attachmentName {
		t.Errorf("attachment Filename = %q, want %q", att.Filename, attachmentName)
	}
	if att.ContentType != attachmentType {
		t.Errorf("attachment ContentType = %q, want %q", att.ContentType, attachmentType)
	}

	content, ref := readBlob(t, inst, att.Blob.ID)
	if content != string(attachmentBody) {
		t.Errorf("blob content = %q, want %q", content, attachmentBody)
	}
	if ref.Size != int64(len(attachmentBody)) {
		t.Errorf("blob Ref.Size = %d, want %d", ref.Size, len(attachmentBody))
	}
}
