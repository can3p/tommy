//go:build integration

// Mailjet integration tests point the real mailjet-apiv3-go/v4 SDK at a live
// tommy and check both sides of the contract: that the SDK is satisfied by
// tommy's response (it parses Mailjet's real success envelope, including the
// per-recipient MessageID/MessageUUID fan-out) and that what tommy actually
// captured matches what was sent.
//
// The one sharp edge documented in docs/clients.md: SendMailV31 builds its
// URL as apiBase + ".1/send", so the base URL passed to NewMailjetClient must
// already end in "/v3" for the arithmetic to land on tommy's mounted
// "/v3.1/send" route.
package integration

import (
	"testing"
	"time"

	mailjet "github.com/mailjet/mailjet-apiv3-go/v4"

	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	mailjetprovider "github.com/can3p/tommy/plugins/mail/providers/mailjet"
)

// mailjetClient builds an SDK client pointed at inst, with the "/v3" suffix
// the SDK's own URL arithmetic requires - see the package doc above.
func mailjetClient(inst *testutil.Instance) *mailjet.Client {
	return mailjet.NewMailjetClient("any-key", "any-secret", inst.IngressURL+"/v3")
}

// findMailEvent returns the mail event whose subject matches, waiting for it
// to appear. It exists because the store lists newest-first and this suite
// often sends several messages in one request, so index position is not a
// reliable way to tell them apart.
func findMailEvent(t *testing.T, inst *testutil.Instance, provider, subject string, timeout time.Duration) *mail.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, e := range inst.Events(store.Query{Plugin: mail.PluginName, Provider: provider}) {
			if m, ok := mail.MessageOf(e); ok && m.Subject == subject {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event with subject %q appeared within %s", provider, subject, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMailjetSDKSendsAndIsCaptured sends a single message to two recipients
// through the real SDK and checks that the SDK parses tommy's success
// envelope - Status, and a distinct MessageID/MessageUUID per recipient -
// and that tommy recorded exactly one event carrying both recipients and the
// right content.
func TestMailjetSDKSendsAndIsCaptured(t *testing.T) {
	inst := startTommy(t)
	client := mailjetClient(inst)

	messages := mailjet.MessagesV31{
		Info: []mailjet.InfoMessagesV31{{
			From: &mailjet.RecipientV31{Email: "alice@example.com", Name: "Alice"},
			To: &mailjet.RecipientsV31{
				{Email: "bob@example.com", Name: "Bob"},
				{Email: "carol@example.com", Name: "Carol"},
			},
			Subject:  "Hello from the mailjet SDK",
			TextPart: "It works.",
		}},
	}

	res, err := client.SendMailV31(&messages)
	if err != nil {
		t.Fatalf("SendMailV31: %v", err)
	}
	if len(res.ResultsV31) != 1 {
		t.Fatalf("got %d results, want 1", len(res.ResultsV31))
	}

	result := res.ResultsV31[0]
	if result.Status != "success" {
		t.Errorf("Status = %q, want %q", result.Status, "success")
	}
	if len(result.To) != 2 {
		t.Fatalf("got %d per-recipient results, want 2", len(result.To))
	}
	seen := map[string]bool{}
	for _, r := range result.To {
		seen[r.Email] = true
		if r.MessageID == 0 {
			t.Errorf("recipient %s: MessageID is zero", r.Email)
		}
		if r.MessageUUID == "" {
			t.Errorf("recipient %s: MessageUUID is empty", r.Email)
		}
	}
	if !seen["bob@example.com"] || !seen["carol@example.com"] {
		t.Errorf("recipient results = %+v, missing bob or carol", result.To)
	}

	m := findMailEvent(t, inst, mailjetprovider.ProviderName, "Hello from the mailjet SDK", 3*time.Second)
	if m.From.Email != "alice@example.com" {
		t.Errorf("captured From = %q, want alice@example.com", m.From.Email)
	}
	if len(m.To) != 2 {
		t.Fatalf("captured %d recipients, want 2", len(m.To))
	}
	if m.Text != "It works." {
		t.Errorf("captured Text = %q, want %q", m.Text, "It works.")
	}
}

// TestMailjetSDKFanOutAndAttachment sends one request carrying two Messages[]
// entries - the real fan-out unit - and checks it lands as exactly two
// events, and that the second message's attachment round-trips through
// tommy's blob store byte for byte.
func TestMailjetSDKFanOutAndAttachment(t *testing.T) {
	inst := startTommy(t)
	client := mailjetClient(inst)

	attachmentBody := "hello from an attachment\n"

	messages := mailjet.MessagesV31{
		Info: []mailjet.InfoMessagesV31{
			{
				From:     &mailjet.RecipientV31{Email: "alice@example.com"},
				To:       &mailjet.RecipientsV31{{Email: "bob@example.com"}},
				Subject:  "Fan-out message one",
				TextPart: "First message.",
			},
			{
				From:     &mailjet.RecipientV31{Email: "alice@example.com"},
				To:       &mailjet.RecipientsV31{{Email: "dave@example.com"}},
				Subject:  "Fan-out message two",
				TextPart: "Second message, with an attachment.",
				Attachments: &mailjet.AttachmentsV31{{
					ContentType:   "text/plain",
					Filename:      "note.txt",
					Base64Content: base64Encode(attachmentBody),
				}},
			},
		},
	}

	res, err := client.SendMailV31(&messages)
	if err != nil {
		t.Fatalf("SendMailV31: %v", err)
	}
	if len(res.ResultsV31) != 2 {
		t.Fatalf("got %d results, want 2", len(res.ResultsV31))
	}
	for i, r := range res.ResultsV31 {
		if r.Status != "success" {
			t.Errorf("result[%d].Status = %q, want success", i, r.Status)
		}
	}

	events := inst.WaitForEvents(2, store.Query{Plugin: mail.PluginName, Provider: mailjetprovider.ProviderName}, 3*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d mailjet events, want exactly 2 (one per Messages[] entry)", len(events))
	}

	one := findMailEvent(t, inst, mailjetprovider.ProviderName, "Fan-out message one", 3*time.Second)
	if one.HasAttachments() {
		t.Errorf("message one should carry no attachment, got %+v", one.Attachments)
	}

	two := findMailEvent(t, inst, mailjetprovider.ProviderName, "Fan-out message two", 3*time.Second)
	if len(two.Attachments) != 1 {
		t.Fatalf("message two: got %d attachments, want 1", len(two.Attachments))
	}
	att := two.Attachments[0]
	if att.Filename != "note.txt" {
		t.Errorf("attachment Filename = %q, want note.txt", att.Filename)
	}
	if att.ContentType != "text/plain" {
		t.Errorf("attachment ContentType = %q, want text/plain", att.ContentType)
	}

	got, ref := readBlob(t, inst, att.Blob.ID)
	if got != attachmentBody {
		t.Errorf("blob content = %q, want %q", got, attachmentBody)
	}
	if ref.Size != int64(len(attachmentBody)) {
		t.Errorf("blob Ref.Size = %d, want %d", ref.Size, len(attachmentBody))
	}
}
