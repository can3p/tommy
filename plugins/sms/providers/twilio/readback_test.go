package twilio_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
)

// e2eAccountSid is used for the full-stack tests, distinct from the unit test
// constant so a copy-paste mistake between the two files cannot hide a bug.
const e2eAccountSid = "ACe2e00000000000000000000000000000"

func postFormLive(t *testing.T, in *testutil.Instance, values url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, in.Ingress("/2010-04-01/Accounts/"+e2eAccountSid+"/Messages.json"),
		strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestEndToEndCreateListFetch drives the provider through a real HTTP round
// trip (the shape an actual twilio-go client or clienthelp.RoundTripper user
// gets), and proves list and fetch are served from the store: whatever create
// wrote is what a second, independent request reads back.
func TestEndToEndCreateListFetch(t *testing.T) {
	in := testutil.Start(t, nil, sms.New(sms.WithProviders(twilio.New())))

	status, body := postFormLive(t, in, url.Values{
		"To":   {"+15558675310"},
		"From": {"+15557122661"},
		"Body": {"Round trip."},
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", status, body)
	}
	var created apiResource
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Sid == "" || created.Status != "queued" {
		t.Fatalf("unexpected create response: %+v", created)
	}

	// List must include the message this account just created.
	status, body = in.GetBody(in.Ingress("/2010-04-01/Accounts/" + e2eAccountSid + "/Messages.json"))
	if status != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", status, body)
	}
	var listResp struct {
		Messages []apiResource `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Messages) != 1 {
		t.Fatalf("list returned %d messages, want 1: %+v", len(listResp.Messages), listResp.Messages)
	}
	if listResp.Messages[0].Sid != created.Sid {
		t.Errorf("listed sid = %q, want %q", listResp.Messages[0].Sid, created.Sid)
	}
	if !resourceEqual(listResp.Messages[0], created) {
		t.Errorf("list entry does not match the create response:\n got  = %s\n want = %s",
			dump(listResp.Messages[0]), dump(created))
	}

	// Fetch by Sid, with the real Twilio ".json" suffix, must return the
	// identical resource - an SDK that creates then fetches sees its own
	// write.
	status, body = in.GetBody(in.Ingress("/2010-04-01/Accounts/" + e2eAccountSid + "/Messages/" + created.Sid + ".json"))
	if status != http.StatusOK {
		t.Fatalf("fetch status = %d, body = %s", status, body)
	}
	var fetched apiResource
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("decode fetch response: %v", err)
	}
	if !resourceEqual(fetched, created) {
		t.Errorf("fetch does not match the create response:\n got  = %s\n want = %s", dump(fetched), dump(created))
	}

	// The same fetch without the suffix must work identically: the wildcard
	// captures the whole segment either way.
	status, body = in.GetBody(in.Ingress("/2010-04-01/Accounts/" + e2eAccountSid + "/Messages/" + created.Sid))
	if status != http.StatusOK {
		t.Fatalf("fetch without suffix status = %d, body = %s", status, body)
	}
	var fetchedNoSuffix apiResource
	if err := json.Unmarshal([]byte(body), &fetchedNoSuffix); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resourceEqual(fetchedNoSuffix, created) {
		t.Errorf("fetch without .json suffix mismatch:\n got  = %s\n want = %s", dump(fetchedNoSuffix), dump(created))
	}
}

// TestFetchNotFound covers a Sid this provider never minted, and a
// syntactically plausible but unknown one, both reporting Twilio's own 404
// shape.
func TestFetchNotFound(t *testing.T) {
	in := testutil.Start(t, nil, sms.New(sms.WithProviders(twilio.New())))

	for _, sid := range []string{"nonsense", "SMdoes-not-exist", "MMdoes-not-exist"} {
		status, body := in.GetBody(in.Ingress("/2010-04-01/Accounts/" + e2eAccountSid + "/Messages/" + sid + ".json"))
		if status != http.StatusNotFound {
			t.Fatalf("sid %q: status = %d, body = %s", sid, status, body)
		}
		var got apiError
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("sid %q: decode: %v", sid, err)
		}
		if got.Code != 20404 || got.Status != 404 {
			t.Errorf("sid %q: got %+v, want code 20404 status 404", sid, got)
		}
	}
}

// TestListDoesNotLeakForeignEvents proves the read-back surfaces only ever
// show what this provider itself recorded: neither another plugin's event nor
// another sms provider's message shows up in Twilio's own list.
func TestListDoesNotLeakForeignEvents(t *testing.T) {
	in := testutil.Start(t, nil, sms.New(sms.WithProviders(twilio.New())))

	// A message this provider actually created.
	status, body := postFormLive(t, in, url.Values{
		"To":   {"+15558675310"},
		"From": {"+15557122661"},
		"Body": {"Mine."},
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", status, body)
	}

	// An event from an entirely different plugin.
	foreignPlugin := &event.Event{
		Plugin:   "mail",
		Provider: "sendgrid",
		Type:     "mail.message",
		Summary:  event.Summary{Title: "not sms at all"},
		Raw:      event.Raw{Transport: "http", Text: true},
	}
	if err := in.Store.Append(context.Background(), foreignPlugin); err != nil {
		t.Fatalf("append foreign plugin event: %v", err)
	}

	// An sms.message from a different provider of the same plugin.
	otherMsg := &sms.Message{From: "+15550000000", To: "+15551111111", Body: "not twilio's"}
	otherMsg.Normalize()
	foreignProvider := &event.Event{
		Plugin:   sms.Name,
		Provider: "fake",
		Type:     sms.EventType,
		Summary:  otherMsg.EventSummary(),
		Payload:  otherMsg,
		Raw:      event.Raw{Transport: "http", Text: true},
	}
	if err := in.Store.Append(context.Background(), foreignProvider); err != nil {
		t.Fatalf("append foreign provider event: %v", err)
	}

	status, body = in.GetBody(in.Ingress("/2010-04-01/Accounts/" + e2eAccountSid + "/Messages.json"))
	if status != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", status, body)
	}
	var listResp struct {
		Messages []apiResource `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listResp.Messages) != 1 {
		t.Fatalf("list returned %d messages, want exactly the one this provider created: %+v",
			len(listResp.Messages), listResp.Messages)
	}
	if listResp.Messages[0].Body != "Mine." {
		t.Errorf("list returned the wrong message: %+v", listResp.Messages[0])
	}

	// Confirm via the store directly too, scoped the way the sms plugin's own
	// event helpers would see it.
	all, err := in.Store.List(context.Background(), store.Query{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("store has %d events, want 3 (one twilio, one foreign plugin, one foreign provider)", len(all))
	}
}
