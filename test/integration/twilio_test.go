//go:build integration

// Twilio integration tests are the important ones in this suite: twilio-go
// has no base-URL override at all (docs/clients.md), so the only way to
// point it at tommy is clienthelp's *http.Client swap, injected through
// twilio.NewRestClientWithParams exactly as the docs show. Getting a 201 is
// necessary but not sufficient - the real proof of fidelity is that the SDK
// successfully decodes the response into its own generated struct, which is
// what catches shape mismatches like a quoted-string num_segments or a
// field that must be JSON null rather than absent. A read-back through the
// SDK (FetchMessage/ListMessage) is the strongest single check: it proves
// tommy's list and fetch routes match the create route closely enough that
// one client library is happy with all three.
package integration

import (
	"testing"
	"time"

	"github.com/twilio/twilio-go"
	twclient "github.com/twilio/twilio-go/client"
	openapi "github.com/twilio/twilio-go/rest/api/v2010"

	"github.com/can3p/tommy/clienthelp"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/sms"
	twilioprovider "github.com/can3p/tommy/plugins/sms/providers/twilio"
)

const (
	twilioAccountSid = "ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	twilioAuthToken  = "authtokenxxxxxxxxxxxxxxxxxxxxxxxx" // any value; tommy records it
)

// twilioRestClient wires up twilio-go exactly as docs/clients.md documents:
// a client.Client carrying clienthelp.HTTPClient(ingressURL), passed through
// NewRestClientWithParams. Nothing about rc.Api or the generated params
// types is aware its requests land on tommy instead of api.twilio.com.
func twilioRestClient(inst *testutil.Instance) *twilio.RestClient {
	tc := &twclient.Client{
		Credentials: twclient.NewCredentials(twilioAccountSid, twilioAuthToken),
		HTTPClient:  clienthelp.HTTPClient(inst.IngressURL),
	}
	tc.SetAccountSid(twilioAccountSid)
	return twilio.NewRestClientWithParams(twilio.ClientParams{Client: tc})
}

// TestTwilioSDKCreateMessageIsParsedFaithfully sends a message through
// CreateMessage and checks every field the SDK's own struct exposes,
// including the two shapes that are easy for a fake to get wrong: quoted
// integers (num_segments, num_media) and fields that must decode as JSON
// null rather than a zero value (error_code, price).
func TestTwilioSDKCreateMessageIsParsedFaithfully(t *testing.T) {
	inst := startTommy(t)
	rc := twilioRestClient(inst)

	params := &openapi.CreateMessageParams{}
	params.SetTo("+15558675310")
	params.SetFrom("+15557122661")
	params.SetBody("It works.")

	msg, err := rc.Api.CreateMessage(params)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	requireStr := func(name string, p *string) string {
		t.Helper()
		if p == nil {
			t.Fatalf("%s is nil, want a value", name)
		}
		return *p
	}

	sid := requireStr("Sid", msg.Sid)
	if sid == "" {
		t.Error("Sid is empty")
	}
	if status := requireStr("Status", msg.Status); status == "" {
		t.Error("Status is empty")
	}
	if to := requireStr("To", msg.To); to != "+15558675310" {
		t.Errorf("To = %q, want +15558675310", to)
	}
	if from := requireStr("From", msg.From); from != "+15557122661" {
		t.Errorf("From = %q, want +15557122661", from)
	}
	if body := requireStr("Body", msg.Body); body != "It works." {
		t.Errorf("Body = %q, want %q", body, "It works.")
	}
	// The sharp edge: Twilio's real API quotes these integers, so the SDK
	// declares them as *string, not *int/*json.Number. A fake that emitted
	// bare JSON numbers here would still return 201, but the SDK's decoder
	// would leave these nil (mismatched type -> decode error surfaces
	// earlier, at CreateMessage's err return above) or silently zero.
	if numSegments := requireStr("NumSegments", msg.NumSegments); numSegments != "1" {
		t.Errorf("NumSegments = %q, want %q (a short GSM-7 body is one segment)", numSegments, "1")
	}
	if numMedia := requireStr("NumMedia", msg.NumMedia); numMedia != "0" {
		t.Errorf("NumMedia = %q, want %q", numMedia, "0")
	}
	// These must be present-but-null, not simply absent: an SDK struct field
	// left at its Go zero value (nil pointer) looks identical whether the
	// server sent JSON null or omitted the key, so this only proves tommy
	// didn't send a non-null placeholder that would fail to decode.
	if msg.ErrorCode != nil {
		t.Errorf("ErrorCode = %v, want nil", *msg.ErrorCode)
	}
	if msg.ErrorMessage != nil {
		t.Errorf("ErrorMessage = %v, want nil", *msg.ErrorMessage)
	}
	if msg.Price != nil {
		t.Errorf("Price = %v, want nil", *msg.Price)
	}
	if accountSid := requireStr("AccountSid", msg.AccountSid); accountSid != twilioAccountSid {
		t.Errorf("AccountSid = %q, want %q", accountSid, twilioAccountSid)
	}

	// And tommy's own store agrees with what the SDK was handed.
	events := inst.WaitForEvents(1, store.Query{Plugin: sms.Name, Provider: twilioprovider.Name}, 3*time.Second)
	captured, ok := sms.MessageOf(events[0])
	if !ok {
		t.Fatalf("captured event does not carry an sms.Message")
	}
	if captured.To != "+15558675310" || captured.From != "+15557122661" || captured.Body != "It works." {
		t.Errorf("captured message = %+v, want To/From/Body to match what was sent", captured)
	}

	// FetchMessage is the strongest single check: the SDK reads its own
	// write back through a different route (GET vs POST) and must decode it
	// with the same struct.
	fetched, err := rc.Api.FetchMessage(sid, &openapi.FetchMessageParams{})
	if err != nil {
		t.Fatalf("FetchMessage(%s): %v", sid, err)
	}
	if fetched.Sid == nil || *fetched.Sid != sid {
		t.Errorf("FetchMessage Sid = %v, want %q", fetched.Sid, sid)
	}
	if fetched.Body == nil || *fetched.Body != "It works." {
		t.Errorf("FetchMessage Body = %v, want %q", fetched.Body, "It works.")
	}
	if fetched.NumSegments == nil || *fetched.NumSegments != "1" {
		t.Errorf("FetchMessage NumSegments = %v, want %q", fetched.NumSegments, "1")
	}

	// ListMessage exercises the third route (GET .../Messages.json with no
	// Sid) through the same generated struct.
	listed, err := rc.Api.ListMessage(&openapi.ListMessageParams{})
	if err != nil {
		t.Fatalf("ListMessage: %v", err)
	}
	var found bool
	for _, m := range listed {
		if m.Sid != nil && *m.Sid == sid {
			found = true
			if m.Body == nil || *m.Body != "It works." {
				t.Errorf("ListMessage entry Body = %v, want %q", m.Body, "It works.")
			}
		}
	}
	if !found {
		t.Errorf("ListMessage did not include Sid %s among %d messages", sid, len(listed))
	}
}

// TestTwilioSDKMultipleMessagesListInOrder sends three messages and checks
// ListMessage - not just CreateMessage - decodes all three, newest first,
// matching Twilio's own documented list order.
func TestTwilioSDKMultipleMessagesListInOrder(t *testing.T) {
	inst := startTommy(t)
	rc := twilioRestClient(inst)

	var sids []string
	for i, body := range []string{"first", "second", "third"} {
		params := &openapi.CreateMessageParams{}
		params.SetTo("+15558675310")
		params.SetFrom("+15557122661")
		params.SetBody(body)
		msg, err := rc.Api.CreateMessage(params)
		if err != nil {
			t.Fatalf("CreateMessage[%d]: %v", i, err)
		}
		if msg.Sid == nil {
			t.Fatalf("CreateMessage[%d]: Sid is nil", i)
		}
		sids = append(sids, *msg.Sid)
	}

	inst.WaitForEvents(3, store.Query{Plugin: sms.Name, Provider: twilioprovider.Name}, 3*time.Second)

	listed, err := rc.Api.ListMessage(&openapi.ListMessageParams{})
	if err != nil {
		t.Fatalf("ListMessage: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("ListMessage returned %d messages, want 3", len(listed))
	}
	// Newest first: the third message created should be the first listed.
	if listed[0].Sid == nil || *listed[0].Sid != sids[2] {
		t.Errorf("ListMessage[0].Sid = %v, want the last-created %q", listed[0].Sid, sids[2])
	}
	if listed[2].Sid == nil || *listed[2].Sid != sids[0] {
		t.Errorf("ListMessage[2].Sid = %v, want the first-created %q", listed[2].Sid, sids[0])
	}
}
