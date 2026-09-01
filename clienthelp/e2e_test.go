package clienthelp_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/clienthelp"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/all"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/smtp"
	"github.com/can3p/tommy/plugins/sms"
)

// TestEndToEndAgainstTwilioProviderFake is the scenario this package exists
// for: twilio-go has no base-URL hook at all, only a custom *http.Client
// (twilio.NewRestClientWithParams(ClientParams{Client: ...})). This test
// stands in for that SDK with a plain *http.Client built by
// clienthelp.HTTPClient, addressing the request at the real vendor hostname
// exactly as twilio-go's RequestHandler.BuildUrl would, and proves the
// transport gets it to tommy's Twilio fake instead - and that the fake
// records it as a real event.
func TestEndToEndAgainstTwilioProviderFake(t *testing.T) {
	// all.Plugins() includes the SMTP provider, which binds a fixed port
	// (1025) unless told otherwise; pin it to an ephemeral one so this test
	// stays hermetic and safe to run alongside others.
	cfg := config.Ephemeral()
	cfg.SetProvider(mail.PluginName, smtp.ProviderName, config.NewProviderConfig(map[string]any{"port": 0}))

	in := testutil.Start(t, cfg, all.Plugins()...)

	client := clienthelp.HTTPClient(in.IngressURL)

	form := url.Values{
		"To":   {"+15558675310"},
		"From": {"+15557122661"},
		"Body": {"It works via clienthelp."},
	}
	// Addressed at the real Twilio hostname, the way twilio-go itself would
	// build the request - clienthelp.Transport is what redirects this to
	// tommy without twilio-go ever knowing.
	req, err := http.NewRequest(http.MethodPost,
		"https://api.twilio.com/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "authtokenxxxxxxxxxxxxxxxxxxxxxxxx")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	events := in.WaitForEvents(1, store.Query{Plugin: sms.Name, Provider: "twilio"}, 2*time.Second)
	e := events[0]
	if e.Summary.Snippet != "It works via clienthelp." && e.Summary.Title != "It works via clienthelp." {
		t.Errorf("event summary = %+v, want the message body somewhere in it", e.Summary)
	}
}
