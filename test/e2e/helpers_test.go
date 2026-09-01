package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	memory "github.com/can3p/tommy/core/store/memory"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/all"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/mailjet"
	"github.com/can3p/tommy/plugins/mail/providers/sendgrid"
	mailsmtp "github.com/can3p/tommy/plugins/mail/providers/smtp"
)

// twilioAccountSid is used as both the path segment and the presented
// Basic-auth user in every twilio request these tests send - twilio accepts
// any credentials by default, so any well-formed sid does.
const twilioAccountSid = "ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

// startAllPlugins boots every shipped plugin and provider on ephemeral ports.
//
// config.Ephemeral only zeroes the three core listeners; a listener provider
// still falls back to its own package default port when its own config
// section says nothing, which for mail/smtp is the well-known 1025
// (plugins/mail/providers/smtp.DefaultPort). Left alone that collides with a
// real mailhog-style catcher, or with another test process, on any machine
// that has one running - so this pins smtp's port to 0 explicitly, exactly
// as plugins/mail/providers/smtp's own tests do, to keep every listener in
// this suite truly ephemeral.
func startAllPlugins(t *testing.T) *testutil.Instance {
	t.Helper()
	cfg := config.Ephemeral()
	cfg.SetProvider(mail.PluginName, smtpProviderName, config.NewProviderConfig(map[string]any{"port": 0}))
	return testutil.Start(t, cfg, all.Plugins()...)
}

// smtpProviderName avoids an import collision between plugins/mail's smtp
// provider package and the standard library's net/smtp, both used in this
// file.
const smtpProviderName = mailsmtp.ProviderName

// sendMailjet posts a Mailjet v3.1 send request and fails the test unless it
// is accepted with Mailjet's real 200 success envelope.
func sendMailjet(t *testing.T, inst *testutil.Instance, from, to, subject, text string) {
	t.Helper()
	body := fmt.Sprintf(`{"Messages":[{"From":{"Email":%q},"To":[{"Email":%q}],"Subject":%q,"TextPart":%q}]}`,
		from, to, subject, text)
	req, err := http.NewRequest(http.MethodPost, inst.Ingress(mailjet.SendPath), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build mailjet request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("any-key", "any-secret")
	resp := inst.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mailjet send: status %d", resp.StatusCode)
	}
}

// sendSendgrid posts a SendGrid v3 Mail Send request and fails the test
// unless it is accepted with the real 202-empty-body contract.
func sendSendgrid(t *testing.T, inst *testutil.Instance, from, to, subject, text string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"personalizations":[{"to":[{"email":%q}],"subject":%q}],"from":{"email":%q},"content":[{"type":"text/plain","value":%q}]}`,
		to, subject, from, text)
	req, err := http.NewRequest(http.MethodPost, inst.Ingress(sendgrid.SendPath), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build sendgrid request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer SG.fake-key")
	resp := inst.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("sendgrid send: status %d", resp.StatusCode)
	}
}

// sendTwilio posts a form-encoded Twilio Message create request and fails
// the test unless it is accepted with the real 201 resource.
func sendTwilio(t *testing.T, inst *testutil.Instance, from, to, body string) {
	t.Helper()
	form := url.Values{"To": {to}, "From": {from}, "Body": {body}}.Encode()
	path := fmt.Sprintf("/2010-04-01/Accounts/%s/Messages.json", twilioAccountSid)
	req, err := http.NewRequest(http.MethodPost, inst.Ingress(path), strings.NewReader(form))
	if err != nil {
		t.Fatalf("build twilio request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(twilioAccountSid, "any-token")
	resp := inst.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("twilio send: status %d", resp.StatusCode)
	}
}

// sendSMTP delivers a message over real SMTP with the standard library
// client - exactly the path an application under test takes.
func sendSMTP(t *testing.T, addr, from, to, subject, text string) {
	t.Helper()
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n", from, to, subject, text)
	if err := smtp.SendMail(addr, nil, from, []string{to}, []byte(msg)); err != nil {
		t.Fatalf("smtp send: %v", err)
	}
}

// waitForSMTPAddr polls the live snippet context until the smtp listener has
// bound and published its address. It runs in its own goroutine
// (core/server.Start starts every ListenerProvider concurrently), so it is
// not guaranteed to be ready the instant Start returns.
func waitForSMTPAddr(t *testing.T, inst *testutil.Instance, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if addr := inst.Server.SnippetCtx().SMTPAddr; addr != "" {
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("smtp listener address never resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForSubscriber blocks until the store has at least one live SSE
// subscriber, so a test can append events only after the stream is actually
// listening - Subscribe delivery is best-effort and carries no backlog, so
// an event appended before anyone is subscribed is simply never seen.
func waitForSubscriber(t *testing.T, inst *testutil.Instance, timeout time.Duration) {
	t.Helper()
	ms, ok := inst.Store.(*memory.Store)
	if !ok {
		// A non-default store: give the connection a moment and move on
		// rather than depending on an internal counter it does not have.
		time.Sleep(50 * time.Millisecond)
		return
	}
	deadline := time.Now().Add(timeout)
	for ms.Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no SSE subscriber registered in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// decodedEvent is a minimal shape for asserting plugin/provider/type on a
// GET /api/v1/events response, without pulling in core/event.Event's full
// shape, which this test has no need of.
type decodedEvent struct {
	Plugin   string `json:"plugin"`
	Provider string `json:"provider"`
	Type     string `json:"type"`
}

func decodeEventList(t *testing.T, body string) []decodedEvent {
	t.Helper()
	var out []decodedEvent
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode events %q: %v", body, err)
	}
	return out
}
