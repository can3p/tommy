package e2e_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/store"
)

// TestCrossPluginTrafficLandsInOneStore drives all four shipped providers -
// two mail HTTP providers, a real SMTP session and Twilio's SMS API - against
// one running tommy and checks every one of them ends up in the same store,
// tagged with the plugin and provider that actually produced it.
func TestCrossPluginTrafficLandsInOneStore(t *testing.T) {
	inst := startAllPlugins(t)

	smtpAddr := waitForSMTPAddr(t, inst, 3*time.Second)

	sendMailjet(t, inst, "alice@example.com", "bob@example.com", "Hello from mailjet", "via mailjet")
	sendSendgrid(t, inst, "alice@example.com", "bob@example.com", "Hello from sendgrid", "via sendgrid")
	sendSMTP(t, smtpAddr, "alice@example.com", "bob@example.com", "Hello from smtp", "via smtp")
	sendTwilio(t, inst, "+15557122661", "+15558675310", "via-twilio")

	events := inst.WaitForEvents(4, store.Query{}, 5*time.Second)
	if len(events) != 4 {
		t.Fatalf("got %d events, want exactly 4", len(events))
	}

	seen := map[string]map[string]bool{}
	for _, e := range events {
		if seen[e.Plugin] == nil {
			seen[e.Plugin] = map[string]bool{}
		}
		seen[e.Plugin][e.Provider] = true
	}
	for _, want := range []struct{ plugin, provider string }{
		{"mail", "mailjet"}, {"mail", "sendgrid"}, {"mail", "smtp"}, {"sms", "twilio"},
	} {
		if !seen[want.plugin][want.provider] {
			t.Errorf("no event recorded for %s/%s; saw %v", want.plugin, want.provider, seen)
		}
	}

	if mail := inst.Events(store.Query{Plugin: "mail"}); len(mail) != 3 {
		t.Errorf("mail events = %d, want 3", len(mail))
	}
	if sms := inst.Events(store.Query{Plugin: "sms"}); len(sms) != 1 {
		t.Errorf("sms events = %d, want 1", len(sms))
	}
}

// TestPerPluginAPIIsolation checks that each plugin's own read-back API, and
// the generic event feed scoped by plugin, only ever returns its own
// messages - an sms query must never surface a mail message and vice versa.
func TestPerPluginAPIIsolation(t *testing.T) {
	inst := startAllPlugins(t)

	sendMailjet(t, inst, "alice@example.com", "bob@example.com", "Only for mail", "mail-only-marker")
	sendTwilio(t, inst, "+15557122661", "+15558675310", "sms-only-marker")
	inst.WaitForEvents(2, store.Query{}, 5*time.Second)

	status, body := inst.GetBody(inst.API("/sms/messages"))
	if status != http.StatusOK {
		t.Fatalf("GET /sms/messages: status %d", status)
	}
	if strings.Contains(body, "mail-only-marker") {
		t.Errorf("sms API leaked a mail message: %s", body)
	}
	if !strings.Contains(body, "sms-only-marker") {
		t.Errorf("sms API is missing its own message: %s", body)
	}

	status, body = inst.GetBody(inst.API("/mail/messages"))
	if status != http.StatusOK {
		t.Fatalf("GET /mail/messages: status %d", status)
	}
	if strings.Contains(body, "sms-only-marker") {
		t.Errorf("mail API leaked an sms message: %s", body)
	}
	if !strings.Contains(body, "mail-only-marker") {
		t.Errorf("mail API is missing its own message: %s", body)
	}

	status, body = inst.GetBody(inst.API("/events?plugin=sms"))
	if status != http.StatusOK {
		t.Fatalf("GET /events?plugin=sms: status %d", status)
	}
	for _, e := range decodeEventList(t, body) {
		if e.Plugin != "sms" {
			t.Errorf("events?plugin=sms returned a %s event", e.Plugin)
		}
	}
	if !strings.Contains(body, "sms-only-marker") {
		t.Errorf("events?plugin=sms is missing the sms message: %s", body)
	}
}

// TestSSEStreamCarriesMultiplePlugins opens the same SSE connection the UI
// uses and checks that events from two different plugins both arrive on it -
// the stream is not scoped to one plugin's tab by default.
func TestSSEStreamCarriesMultiplePlugins(t *testing.T) {
	inst := startAllPlugins(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.API("/events/stream"), nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	resp, err := inst.Client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: status %d", resp.StatusCode)
	}

	waitForSubscriber(t, inst, 2*time.Second)

	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	sendMailjet(t, inst, "alice@example.com", "bob@example.com", "SSE mail", "sse-mail-marker")
	sendTwilio(t, inst, "+15557122661", "+15558675310", "sse-sms-marker")

	var sawMail, sawSMS bool
	for !sawMail || !sawSMS {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed before seeing both plugins (mail=%v sms=%v)", sawMail, sawSMS)
			}
			data, isData := strings.CutPrefix(line, "data: ")
			if !isData {
				continue
			}
			if strings.Contains(data, `"plugin":"mail"`) {
				sawMail = true
			}
			if strings.Contains(data, `"plugin":"sms"`) {
				sawSMS = true
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for both plugins on the SSE stream (mail=%v sms=%v)", sawMail, sawSMS)
		}
	}
}

// TestDeleteScopedToPluginLeavesOtherIntact checks that DELETE
// /api/v1/events?plugin=mail clears only mail, leaving sms events in place.
func TestDeleteScopedToPluginLeavesOtherIntact(t *testing.T) {
	inst := startAllPlugins(t)

	sendMailjet(t, inst, "alice@example.com", "bob@example.com", "to be deleted", "delete-me")
	sendTwilio(t, inst, "+15557122661", "+15558675310", "keep-me")
	inst.WaitForEvents(2, store.Query{}, 5*time.Second)

	req, err := http.NewRequest(http.MethodDelete, inst.API("/events?plugin=mail"), nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp := inst.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /events?plugin=mail: status %d", resp.StatusCode)
	}

	if mail := inst.Events(store.Query{Plugin: "mail"}); len(mail) != 0 {
		t.Errorf("mail events survived a scoped delete: %d", len(mail))
	}
	sms := inst.Events(store.Query{Plugin: "sms"})
	if len(sms) != 1 {
		t.Fatalf("sms events affected by a mail-scoped delete: got %d, want 1", len(sms))
	}
	if sms[0].Provider != "twilio" {
		t.Errorf("unexpected surviving sms event: %+v", sms[0])
	}
}
