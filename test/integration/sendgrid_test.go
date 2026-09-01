//go:build integration

// SendGrid integration tests build a payload with the SDK's own helpers
// (mail.NewV3MailInit, Personalization, Attachment) rather than hand-written
// JSON, and send it with sendgrid.GetRequest + sendgrid.API - the documented
// way around NewSendClient's hardcoded host (docs/clients.md). They assert
// the SDK sees the real 202-empty-body-plus-X-Message-Id contract, and that
// personalizations fan out into the right events with the right content.
package integration

import (
	"testing"
	"time"

	"github.com/sendgrid/rest"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"

	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	tommymail "github.com/can3p/tommy/plugins/mail"
	sendgridprovider "github.com/can3p/tommy/plugins/mail/providers/sendgrid"
)

// sendgridSend posts m through the real SDK's GetRequest/API pair, the way
// docs/clients.md documents as the fix for NewSendClient's hardcoded host.
func sendgridSend(t *testing.T, inst *testutil.Instance, m *mail.SGMailV3) *rest.Response {
	t.Helper()
	req := sendgrid.GetRequest("SG.fake-key", sendgridprovider.SendPath, inst.IngressURL)
	req.Method = "POST"
	req.Body = mail.GetRequestBody(m)

	resp, err := sendgrid.API(req)
	if err != nil {
		t.Fatalf("sendgrid.API: %v", err)
	}
	return resp
}

// findSendgridEvent is findMailEvent scoped to the sendgrid provider.
func findSendgridEvent(t *testing.T, inst *testutil.Instance, subject string, timeout time.Duration) *tommymail.Message {
	t.Helper()
	return findMailEvent(t, inst, sendgridprovider.ProviderName, subject, timeout)
}

// TestSendGridSDKSendsAndIsCaptured builds a single-personalization message
// with the SDK's own builders and checks both that the SDK sees the real
// 202/X-Message-Id contract and that tommy captured the content correctly.
func TestSendGridSDKSendsAndIsCaptured(t *testing.T) {
	inst := startTommy(t)

	m := mail.NewV3MailInit(
		mail.NewEmail("Alice", "alice@example.com"),
		"Hello from the sendgrid SDK",
		mail.NewEmail("Bob", "bob@example.com"),
		mail.NewContent("text/plain", "It works."),
		mail.NewContent("text/html", "<p>It <b>works</b>.</p>"),
	)

	resp := sendgridSend(t, inst, m)

	if resp.StatusCode != 202 {
		t.Fatalf("StatusCode = %d, want 202; body: %s", resp.StatusCode, resp.Body)
	}
	if resp.Body != "" {
		t.Errorf("Body = %q, want empty", resp.Body)
	}
	ids := resp.Headers["X-Message-Id"]
	if len(ids) == 0 || ids[0] == "" {
		t.Fatalf("X-Message-Id header missing or empty; headers: %+v", resp.Headers)
	}

	got := findSendgridEvent(t, inst, "Hello from the sendgrid SDK", 3*time.Second)
	if got.From.Email != "alice@example.com" {
		t.Errorf("From = %q, want alice@example.com", got.From.Email)
	}
	if len(got.To) != 1 || got.To[0].Email != "bob@example.com" {
		t.Errorf("To = %+v, want [bob@example.com]", got.To)
	}
	if got.Text != "It works." {
		t.Errorf("Text = %q, want %q", got.Text, "It works.")
	}
	if got.HTML != "<p>It <b>works</b>.</p>" {
		t.Errorf("HTML = %q", got.HTML)
	}
}

// TestSendGridSDKPersonalizationsFanOutWithAttachment sends one request
// carrying two personalizations - the SDK's own fan-out unit - plus a shared
// attachment, and checks it lands as exactly two events, each carrying the
// attachment, with content round-tripped through tommy's blob store intact.
func TestSendGridSDKPersonalizationsFanOutWithAttachment(t *testing.T) {
	inst := startTommy(t)

	m := mail.NewV3MailInit(
		mail.NewEmail("Alice", "alice@example.com"),
		"", // overridden per personalization below
		mail.NewEmail("placeholder", "placeholder@example.com"),
		mail.NewContent("text/plain", "Shared body."),
	)
	// NewV3MailInit already added one personalization for the placeholder
	// recipient; replace it with the two this test actually wants.
	m.Personalizations = nil

	p1 := mail.NewPersonalization()
	p1.AddTos(mail.NewEmail("Bob", "bob@example.com"))
	p1.Subject = "Personalization one"

	p2 := mail.NewPersonalization()
	p2.AddTos(mail.NewEmail("Carol", "carol@example.com"))
	p2.Subject = "Personalization two"

	m.AddPersonalizations(p1, p2)

	attachmentBody := "sendgrid attachment body\n"
	att := mail.NewAttachment().
		SetContent(base64Encode(attachmentBody)).
		SetType("text/plain").
		SetFilename("receipt.txt").
		SetDisposition("attachment")
	m.AddAttachment(att)

	resp := sendgridSend(t, inst, m)
	if resp.StatusCode != 202 {
		t.Fatalf("StatusCode = %d, want 202; body: %s", resp.StatusCode, resp.Body)
	}

	events := inst.WaitForEvents(2, store.Query{Plugin: tommymail.PluginName, Provider: sendgridprovider.ProviderName}, 3*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d sendgrid events, want exactly 2 (one per personalization)", len(events))
	}

	one := findSendgridEvent(t, inst, "Personalization one", 3*time.Second)
	if len(one.To) != 1 || one.To[0].Email != "bob@example.com" {
		t.Errorf("personalization one To = %+v, want [bob@example.com]", one.To)
	}
	two := findSendgridEvent(t, inst, "Personalization two", 3*time.Second)
	if len(two.To) != 1 || two.To[0].Email != "carol@example.com" {
		t.Errorf("personalization two To = %+v, want [carol@example.com]", two.To)
	}

	for _, msg := range []*tommymail.Message{one, two} {
		if len(msg.Attachments) != 1 {
			t.Fatalf("message to %v: got %d attachments, want 1", msg.To, len(msg.Attachments))
		}
		content, _ := readBlob(t, inst, msg.Attachments[0].Blob.ID)
		if content != attachmentBody {
			t.Errorf("message to %v: blob content = %q, want %q", msg.To, content, attachmentBody)
		}
	}
}
