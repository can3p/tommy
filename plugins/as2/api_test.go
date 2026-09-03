package as2_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/plugins/as2"
)

func TestAPISingleMessageAndBytes(t *testing.T) {
	in := startWithFixtureIdentity(t)
	resp := post(t, in, "signed.mime", signedReceipt("sha256"))
	_ = resp.Body.Close()
	events := in.WaitForEvents(1, storeQueryAll(), 2*time.Second)
	id := string(events[0].ID)

	var one struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		RawURL  string `json:"raw_url"`
		Message struct {
			From string `json:"from"`
			MDN  struct {
				Disposition string `json:"disposition"`
			} `json:"mdn"`
		} `json:"message"`
	}
	if status := in.GetJSON(in.API("/as2/messages/"+id), &one); status != http.StatusOK {
		t.Fatalf("get status = %d", status)
	}
	if one.ID != id || one.Message.From != "PARTNER" {
		t.Errorf("envelope = %+v", one)
	}
	if !strings.Contains(one.Message.MDN.Disposition, "processed") {
		t.Errorf("the stored MDN record is missing: %+v", one.Message.MDN)
	}

	// The raw route hands back the request exactly as it arrived, and refuses
	// to let a browser sniff it: an AS2 payload is attacker-supplied bytes.
	rawResp := in.Get(in.API("/as2/messages/" + id + "/raw"))
	defer func() { _ = rawResp.Body.Close() }()
	if rawResp.StatusCode != http.StatusOK {
		t.Fatalf("raw status = %d", rawResp.StatusCode)
	}
	if got := rawResp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if cd := rawResp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
}

func TestAPINotFound(t *testing.T) {
	in := startWithFixtureIdentity(t)
	for _, path := range []string{
		"/as2/messages/nope",
		"/as2/messages/nope/raw",
		"/as2/messages/nope/payload",
		"/as2/messages/nope/mdn",
	} {
		resp := in.Get(in.API(path))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestAPIDeleteAndClear(t *testing.T) {
	in := startWithFixtureIdentity(t)
	for range 2 {
		resp := post(t, in, "plain.mime", nil)
		_ = resp.Body.Close()
	}
	events := in.WaitForEvents(2, storeQueryAll(), 2*time.Second)

	req, err := http.NewRequest(http.MethodDelete, in.API("/as2/messages/"+string(events[0].ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if got := in.Events(storeQueryAll()); len(got) != 1 {
		t.Errorf("store holds %d events after deleting one of two", len(got))
	}

	req, err = http.NewRequest(http.MethodDelete, in.API("/as2/messages"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp = in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear status = %d, want 204", resp.StatusCode)
	}
	if got := in.Events(storeQueryAll()); len(got) != 0 {
		t.Errorf("store still holds %d events after a clear", len(got))
	}
}

// The certificate endpoint has to say something useful, not 500, when no
// provider has been enabled and so no certificate exists.
func TestCertificateEndpointBeforeAnyProviderIsEnabled(t *testing.T) {
	p := as2.New()
	in := startPlugin(t, p)

	resp := in.Get(in.API("/as2/certificate"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	var info map[string]any
	if status := in.GetJSON(in.API("/as2/identity"), &info); status != http.StatusOK {
		t.Fatalf("identity status = %d", status)
	}
	if info["configured"] != false || info["ready"] != false {
		t.Errorf("identity = %+v, want an unconfigured identity", info)
	}
}

// A search that matches nothing must come back as an empty list, not null:
// a client iterating the result should not have to special-case it.
func TestAPIEmptyListIsAnArray(t *testing.T) {
	in := startWithFixtureIdentity(t)
	_, body := in.GetBody(in.API("/as2/messages"))
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("empty list = %q, want []", strings.TrimSpace(body))
	}
}

func TestEndpointsAreDeclared(t *testing.T) {
	eps := as2.Endpoints()
	if len(eps) == 0 {
		t.Fatal("Endpoints() is empty")
	}
	for _, e := range eps {
		if e.Method == "" || e.Path == "" || len(e.Description) < 24 {
			t.Errorf("endpoint %+v is not fully described", e)
		}
		if !strings.HasPrefix(e.Path, as2.APIBase) {
			t.Errorf("endpoint %q is not under %q", e.Path, as2.APIBase)
		}
	}
}
