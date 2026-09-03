package as2_test

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/as2"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// The conformance gate: descriptions are real, snippets render, and every
// declared endpoint is actually mounted.
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, as2.New(&fakeProvider{}))
}

// Until the HTTP provider lands, as2.New() has no providers. That is the only
// thing conformance can hold against it, and this test pins that down so the
// day a provider is added nothing else has quietly rotted.
func TestBarePluginOnlyLacksAProvider(t *testing.T) {
	errs := plugintest.CheckPlugin(as2.New())
	if len(errs) != 1 {
		t.Fatalf("CheckPlugin(as2.New()) reported %d problems, want only the missing provider: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "Providers() is empty") {
		t.Errorf("unexpected conformance failure: %v", errs[0])
	}
}

func TestPluginIdentity(t *testing.T) {
	p := as2.New()
	if p.Name() != "as2" {
		t.Errorf("Name() = %q, want as2", p.Name())
	}
	if p.Title() != "AS2" {
		t.Errorf("Title() = %q, want AS2", p.Title())
	}
	if len(p.Description()) < 40 || !strings.Contains(strings.ToLower(p.Description()), "as2") {
		t.Errorf("Description() = %q, want a couple of real sentences", p.Description())
	}
	if got := p.Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() = %v, want an empty non-nil slice until the HTTP provider lands", got)
	}
	names, err := fs.Glob(p.Templates(), "*.html")
	if err != nil {
		t.Fatalf("Templates(): %v", err)
	}
	if len(names) == 0 {
		t.Error("Templates() returned no templates; the tab cannot render")
	}
}

// The requirement that costs fall only on whoever enabled the feature: a plugin
// nobody registered a provider for must not have generated anything.
func TestNoCertificateExistsUntilAProviderIsEnabled(t *testing.T) {
	p := as2.New()
	if p.Identity().Configured() {
		t.Fatal("the identity is configured before any provider registered")
	}
	if p.Identity().Certificate() != nil {
		t.Fatal("a certificate was generated at construction time")
	}
	if info := p.Identity().Info(); info.Ready || info.Source != "unconfigured" {
		t.Errorf("Info() = %+v, want an unconfigured identity", info)
	}
	if _, _, err := p.Identity().KeyPair(); err == nil {
		t.Error("KeyPair() succeeded on an unconfigured identity")
	}
}

// The IdentityBinder seam: constructing the plugin hands the identity to a
// provider that asks for it, without the plugin importing the provider.
func TestIdentityIsBoundToProviders(t *testing.T) {
	prov := &fakeProvider{}
	p := as2.New(prov)
	if prov.identity == nil {
		t.Fatal("BindIdentity was never called")
	}
	if prov.identity != p.Identity() {
		t.Error("the provider was handed a different identity than the plugin holds")
	}
}

// ------------------------------------------------------------ the whole path

// A message posted to a provider is captured, answered with an MDN, and
// readable back through the plugin's own API.
func TestEndToEnd(t *testing.T) {
	in := startWithFixtureIdentity(t)

	resp := post(t, in, "signed_encrypted.mime", signedReceipt("sha256"))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/signed;") {
		t.Errorf("Content-Type = %q, want a signed MDN", ct)
	}
	if got := resp.Header.Get("AS2-From"); got != "TOMMY" {
		t.Errorf("AS2-From = %q, want the request's AS2-To", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read MDN: %v", err)
	}
	if !bytes.Contains(body, []byte("Disposition: automatic-action/MDN-sent-automatically; processed")) {
		t.Errorf("MDN does not report a clean disposition:\n%s", body)
	}

	events := in.WaitForEvents(1, storeQueryAll(), 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("store holds %d events, want 1", len(events))
	}
	id := string(events[0].ID)

	var listed []struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		PayloadURL string `json:"payload_url"`
		MDNURL     string `json:"mdn_url"`
		Message    struct {
			Security struct {
				Signed    bool `json:"signed"`
				Encrypted bool `json:"encrypted"`
			} `json:"security"`
			MIC struct {
				Algorithm string `json:"algorithm"`
				Coverage  string `json:"coverage"`
			} `json:"mic"`
			Payload struct {
				Format string `json:"format"`
			} `json:"payload"`
		} `json:"message"`
	}
	if status := in.GetJSON(in.API("/as2/messages"), &listed); status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if len(listed) != 1 {
		t.Fatalf("API listed %d messages, want 1", len(listed))
	}
	got := listed[0]
	if got.ID != id {
		t.Errorf("listed id = %q, want %q", got.ID, id)
	}
	if !got.Message.Security.Signed || !got.Message.Security.Encrypted {
		t.Errorf("security = %+v, want signed and encrypted", got.Message.Security)
	}
	if got.Message.MIC.Coverage != as2.MICOverSignedContent {
		t.Errorf("MIC coverage = %q", got.Message.MIC.Coverage)
	}
	if got.Message.Payload.Format != as2.FormatX12 {
		t.Errorf("payload format = %q", got.Message.Payload.Format)
	}

	// Rule 7: a read-back endpoint serves from the store, so what was written
	// can be fetched.
	_, payload := in.GetBody(in.APIURL + strings.TrimPrefix(got.PayloadURL, "/api/v1"))
	if !strings.HasPrefix(payload, "ISA*00*") {
		t.Errorf("payload read back = %q, want the decrypted EDI", firstBytes([]byte(payload)))
	}
	_, mdn := in.GetBody(in.APIURL + strings.TrimPrefix(got.MDNURL, "/api/v1"))
	if !strings.Contains(mdn, "message/disposition-notification") {
		t.Errorf("MDN read back does not look like an MDN: %q", firstBytes([]byte(mdn)))
	}
}

// The certificate endpoint is what a partner is pointed at, so it has to serve
// something importable and refuse to sniff.
func TestCertificateEndpoint(t *testing.T) {
	in := startWithFixtureIdentity(t)

	resp, err := in.Client.Get(in.API("/as2/certificate"))
	if err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(body, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Errorf("certificate endpoint served %q", firstBytes(body))
	}

	var info map[string]any
	if status := in.GetJSON(in.API("/as2/identity"), &info); status != http.StatusOK {
		t.Fatalf("identity status = %d", status)
	}
	if info["ready"] != true {
		t.Errorf("identity info = %+v, want ready", info)
	}
	if _, leaked := info["key"]; leaked {
		t.Error("the identity endpoint hands out something called a key")
	}
	raw, _ := json.Marshal(info)
	if bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("the identity endpoint leaked the private key")
	}
}

// The API's own filters, which are how somebody finds the one message that went
// wrong among a hundred that did not.
func TestAPIFilters(t *testing.T) {
	in := startWithFixtureIdentity(t)

	for _, f := range []string{"plain.mime", "signed.mime", "undecryptable.mime"} {
		resp := post(t, in, f, signedReceipt("sha256"))
		_ = resp.Body.Close()
	}
	in.WaitForEvents(3, storeQueryAll(), 2*time.Second)

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?security=signed", 1},
		{"?security=encrypted", 1},
		{"?security=unprotected", 1},
		{"?issue=" + as2.IssueDecryptionFailed, 1},
		{"?from=PARTNER", 3},
		{"?from=nobody", 0},
		{"?format=" + as2.FormatX12, 2},
	} {
		var listed []map[string]any
		if status := in.GetJSON(in.API("/as2/messages"+tc.query), &listed); status != http.StatusOK {
			t.Fatalf("%s: status %d", tc.query, status)
		}
		if len(listed) != tc.want {
			t.Errorf("%q matched %d messages, want %d", tc.query, len(listed), tc.want)
		}
	}
}
