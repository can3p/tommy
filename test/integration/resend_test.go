//go:build integration

// Resend integration tests point the official github.com/resend/resend-go/v4
// SDK at a live tommy through its one exported hook for this - Client.BaseURL,
// a *url.URL - which is the easy case docs/clients.md's Resend section
// describes: no GetRequest-style workaround (sendgrid-go) and no clienthelp
// indirection (twilio-go) needed.
//
// They exercise the sharp edges the provider's own README calls out:
//   - resend-go marshals to/cc/bcc as JSON arrays but reply_to as a bare
//     string, in the same request - the reason the provider carries a custom
//     stringList decoder rather than a plain []string field.
//   - resend-go's Attachment.MarshalJSON encodes content as a JSON array of
//     integers, never base64 - the reason the provider accepts three content
//     spellings rather than one.
//   - a send-then-Get round trip through the SDK, where reply_to always comes
//     back as an array even though it was sent as a bare string.
//   - a batch send, checking the response ids are index-aligned with the
//     request and that tommy captured one event per entry.
package integration

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	resendgo "github.com/resend/resend-go/v4"

	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	resendprovider "github.com/can3p/tommy/plugins/mail/providers/resend"
)

// resendClient builds an SDK client pointed at inst by way of Client.BaseURL.
// Unlike sendgrid-go (no override on NewSendClient) or twilio-go (no override
// at all), resend-go's NewClient exposes BaseURL as a plain exported
// *url.URL field, set after construction - confirmed by reading
// resend-go/v4@v4.3.0's resend.go, not assumed.
func resendClient(t *testing.T, inst *testutil.Instance) *resendgo.Client {
	t.Helper()
	c := resendgo.NewClient("re_fake_key") // any key; tommy just records it
	u, err := url.Parse(inst.IngressURL)
	if err != nil {
		t.Fatalf("parse ingress URL %q: %v", inst.IngressURL, err)
	}
	c.BaseURL = u
	return c
}

// findResendMessage polls the mail plugin's own read-back API -
// GET /api/v1/mail/messages?provider=resend, the exact route
// plugins/mail/providers/resend/README.md points a user at - for a message
// with the given subject. It waits because the provider appends from the
// ingress handler's own goroutine relative to this test.
func findResendMessage(t *testing.T, inst *testutil.Instance, subject string, timeout time.Duration) mail.MessageView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var views []mail.MessageView
		status := inst.GetJSON(inst.API("/mail/messages?provider="+resendprovider.ProviderName), &views)
		if status != http.StatusOK {
			t.Fatalf("GET /mail/messages?provider=%s: status %d", resendprovider.ProviderName, status)
		}
		for _, v := range views {
			if v.Message != nil && v.Message.Subject == subject {
				return v
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no resend message with subject %q appeared within %s", subject, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestResendSDKSendsAndIsCaptured sends a single email through
// Emails.SendWithContext and checks the message tommy captured - read back
// through the mail plugin's own API, not the in-process store - carries the
// right sender, recipient, and both body parts.
func TestResendSDKSendsAndIsCaptured(t *testing.T) {
	inst := startTommy(t)
	client := resendClient(t, inst)

	params := &resendgo.SendEmailRequest{
		From:    "Acme <alice@example.com>",
		To:      []string{"bob@example.com"},
		Subject: "Hello from the resend SDK",
		Html:    "<p>It <b>works</b>.</p>",
		Text:    "It works.",
	}

	sent, err := client.Emails.SendWithContext(context.Background(), params)
	if err != nil {
		t.Fatalf("SendWithContext: %v", err)
	}
	if sent.Id == "" {
		t.Fatal("SendWithContext: Id is empty")
	}

	view := findResendMessage(t, inst, "Hello from the resend SDK", 3*time.Second)
	m := view.Message
	if m.From.Email != "alice@example.com" || m.From.Name != "Acme" {
		t.Errorf("From = %+v, want {Name:Acme Email:alice@example.com}", m.From)
	}
	if len(m.To) != 1 || m.To[0].Email != "bob@example.com" {
		t.Errorf("To = %+v, want [bob@example.com]", m.To)
	}
	if m.HTML != "<p>It <b>works</b>.</p>" {
		t.Errorf("HTML = %q", m.HTML)
	}
	if m.Text != "It works." {
		t.Errorf("Text = %q, want %q", m.Text, "It works.")
	}
}

// TestResendSDKRecipientUnionAndReadBack sends one message using both
// spellings of the recipient union in the same request - to/cc/bcc as arrays,
// reply_to as a bare string, exactly what resend-go's SendEmailRequest
// produces on the wire - and checks both that tommy's canonical model
// resolved every field correctly and that the SDK's own GetWithContext round
// trip agrees, including reply_to coming back as an array.
func TestResendSDKRecipientUnionAndReadBack(t *testing.T) {
	inst := startTommy(t)
	client := resendClient(t, inst)
	ctx := context.Background()

	params := &resendgo.SendEmailRequest{
		From:    "Acme <alice@example.com>",
		To:      []string{"bob@example.com", "carol@example.com"}, // array
		Cc:      []string{"dave@example.com"},                     // array
		Bcc:     []string{"erin@example.com"},                     // array
		ReplyTo: "support@example.com",                            // bare string
		Subject: "Union round trip",
		Text:    "hi",
	}

	sent, err := client.Emails.SendWithContext(ctx, params)
	if err != nil {
		t.Fatalf("SendWithContext: %v", err)
	}
	if sent.Id == "" {
		t.Fatal("SendWithContext: Id is empty")
	}

	// The canonical model tommy captured.
	view := findResendMessage(t, inst, "Union round trip", 3*time.Second)
	m := view.Message
	if len(m.To) != 2 || m.To[0].Email != "bob@example.com" || m.To[1].Email != "carol@example.com" {
		t.Errorf("captured To = %+v, want [bob@example.com carol@example.com]", m.To)
	}
	if len(m.Cc) != 1 || m.Cc[0].Email != "dave@example.com" {
		t.Errorf("captured Cc = %+v, want [dave@example.com]", m.Cc)
	}
	if len(m.Bcc) != 1 || m.Bcc[0].Email != "erin@example.com" {
		t.Errorf("captured Bcc = %+v, want [erin@example.com]", m.Bcc)
	}
	if len(m.ReplyTo) != 1 || m.ReplyTo[0].Email != "support@example.com" {
		t.Errorf("captured ReplyTo = %+v, want [support@example.com]", m.ReplyTo)
	}

	// The SDK's own read-back of the message it just sent.
	got, err := client.Emails.GetWithContext(ctx, sent.Id)
	if err != nil {
		t.Fatalf("GetWithContext: %v", err)
	}
	if got.Subject != "Union round trip" {
		t.Errorf("Get: Subject = %q, want %q", got.Subject, "Union round trip")
	}
	if got.From != "Acme <alice@example.com>" {
		t.Errorf("Get: From = %q, want %q", got.From, "Acme <alice@example.com>")
	}
	if len(got.To) != 2 || got.To[0] != "bob@example.com" || got.To[1] != "carol@example.com" {
		t.Errorf("Get: To = %+v, want [bob@example.com carol@example.com]", got.To)
	}
	if len(got.Cc) != 1 || got.Cc[0] != "dave@example.com" {
		t.Errorf("Get: Cc = %+v, want [dave@example.com]", got.Cc)
	}
	if len(got.Bcc) != 1 || got.Bcc[0] != "erin@example.com" {
		t.Errorf("Get: Bcc = %+v, want [erin@example.com]", got.Bcc)
	}
	// reply_to was sent as a bare string; Resend's real retrieve response -
	// and this provider's - always renders it as an array.
	if len(got.ReplyTo) != 1 || got.ReplyTo[0] != "support@example.com" {
		t.Errorf("Get: ReplyTo = %+v, want [support@example.com] (array even though sent as a bare string)", got.ReplyTo)
	}
}

// TestResendSDKAttachmentRoundTrip sends an attachment through the SDK, whose
// Attachment.MarshalJSON encodes Content as a JSON array of integers rather
// than the base64 string the REST reference documents, and checks the bytes
// tommy stored - fetched straight from the blob store - match exactly what
// was sent.
func TestResendSDKAttachmentRoundTrip(t *testing.T) {
	inst := startTommy(t)
	client := resendClient(t, inst)

	attachmentBody := []byte("hello from a resend-go attachment, sent as an int array\n")

	params := &resendgo.SendEmailRequest{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: "Attachment round trip",
		Text:    "see attached",
		Attachments: []*resendgo.Attachment{
			{
				Content:     attachmentBody,
				Filename:    "note.txt",
				ContentType: "text/plain",
			},
		},
	}

	sent, err := client.Emails.SendWithContext(context.Background(), params)
	if err != nil {
		t.Fatalf("SendWithContext: %v", err)
	}
	if sent.Id == "" {
		t.Fatal("SendWithContext: Id is empty")
	}

	view := findResendMessage(t, inst, "Attachment round trip", 3*time.Second)
	if len(view.Message.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(view.Message.Attachments))
	}
	att := view.Message.Attachments[0]
	if att.Filename != "note.txt" {
		t.Errorf("attachment Filename = %q, want note.txt", att.Filename)
	}
	if att.ContentType != "text/plain" {
		t.Errorf("attachment ContentType = %q, want text/plain", att.ContentType)
	}

	got, ref := readBlob(t, inst, att.Blob.ID)
	if got != string(attachmentBody) {
		t.Errorf("blob content = %q, want %q", got, string(attachmentBody))
	}
	if ref.Size != int64(len(attachmentBody)) {
		t.Errorf("blob Ref.Size = %d, want %d", ref.Size, len(attachmentBody))
	}
}

// TestResendSDKBatchSendCapturesOnePerEntry sends three emails through
// Batch.SendWithContext and checks that the response ids are index-aligned
// with the request - verified by fetching each returned id back through the
// SDK and confirming it names the entry that was at the same position in the
// request, not just that three distinct ids came back - and that tommy
// recorded exactly one event per entry.
func TestResendSDKBatchSendCapturesOnePerEntry(t *testing.T) {
	inst := startTommy(t)
	client := resendClient(t, inst)
	ctx := context.Background()

	reqs := []*resendgo.SendEmailRequest{
		{From: "alice@example.com", To: []string{"bob@example.com"}, Subject: "Batch one", Text: "one"},
		{From: "alice@example.com", To: []string{"carol@example.com"}, Subject: "Batch two", Text: "two"},
		{From: "alice@example.com", To: []string{"dave@example.com"}, Subject: "Batch three", Text: "three"},
	}

	resp, err := client.Batch.SendWithContext(ctx, reqs)
	if err != nil {
		t.Fatalf("Batch.SendWithContext: %v", err)
	}
	if len(resp.Data) != len(reqs) {
		t.Fatalf("got %d batch results, want %d", len(resp.Data), len(reqs))
	}

	seenIDs := map[string]bool{}
	for i, d := range resp.Data {
		if d.Id == "" {
			t.Fatalf("batch entry %d: id is empty", i)
		}
		if seenIDs[d.Id] {
			t.Fatalf("batch entry %d: id %s repeats an earlier entry's id", i, d.Id)
		}
		seenIDs[d.Id] = true

		got, err := client.Emails.GetWithContext(ctx, d.Id)
		if err != nil {
			t.Fatalf("batch entry %d: GetWithContext(%s): %v", i, d.Id, err)
		}
		if got.Subject != reqs[i].Subject {
			t.Errorf("batch entry %d: id %s resolves to subject %q, want %q (ids not index-aligned with the request)",
				i, d.Id, got.Subject, reqs[i].Subject)
		}
		if len(got.To) != 1 || got.To[0] != reqs[i].To[0] {
			t.Errorf("batch entry %d: id %s resolves to To %+v, want [%s]", i, d.Id, got.To, reqs[i].To[0])
		}
	}

	events := inst.WaitForEvents(len(reqs), store.Query{Plugin: mail.PluginName, Provider: resendprovider.ProviderName}, 3*time.Second)
	if len(events) != len(reqs) {
		t.Fatalf("got %d resend events, want exactly %d (one per batch entry)", len(events), len(reqs))
	}
}
