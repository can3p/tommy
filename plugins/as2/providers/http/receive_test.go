package http_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
)

// messageID is deliberately awkward: dots, digits, a plus sign and angle
// brackets. RFC 4130 §7.4.3 requires the MDN's Original-Message-ID to match the
// request's Message-ID exactly, and that field is what a partner's software
// keys its reconciliation on - so it is asserted byte for byte, brackets
// included, rather than by substring on the interesting part.
const messageID = "<20260903.114309.7f2a+edi@partner.example>"

// start boots a real tommy on ephemeral ports with the as2 plugin and this
// provider. The identity is pinned to the lab's tommy key pair so OpenSSL can
// encrypt to a certificate whose private key the server holds.
//
// Every caller pins cert_file/key_file, cert_dir or in_memory, so no test can
// ever write a certificate into the real user config directory.
func start(t *testing.T, extra map[string]any) *testutil.Instance {
	t.Helper()
	requireOpenSSL(t)
	values := map[string]any{
		"cert_file": key("tommy.crt"),
		"key_file":  key("tommy.key"),
	}
	for k, v := range extra {
		values[k] = v
	}
	cfg := config.Ephemeral()
	cfg.SetProvider(as2.Name, as2http.Name, config.NewProviderConfig(values))
	return testutil.Start(t, cfg, as2.New(as2http.New()))
}

// as2Headers is what every sender puts on the request, with extra overriding.
func as2Headers(extra http.Header) http.Header {
	h := http.Header{}
	h.Set("AS2-From", "PARTNER")
	h.Set("AS2-To", "TOMMY")
	h.Set("AS2-Version", "1.1")
	h.Set("Message-ID", messageID)
	h.Set("Disposition-Notification-To", "as2@partner.example")
	for k, vs := range extra {
		h.Del(k)
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	return h
}

// signedReceipt asks for the MDN to come back signed, which is what makes it
// verifiable by OpenSSL.
func signedReceipt(h http.Header) http.Header {
	h.Set("Disposition-Notification-Options",
		"signed-receipt-protocol=optional,pkcs7-signature; signed-receipt-micalg=optional,sha256")
	return h
}

// post sends bytes to the AS2 route over the instance's real socket.
func post(t *testing.T, in *testutil.Instance, h http.Header, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, in.Ingress("/as2"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header = as2Headers(h)
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	mdn, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read MDN: %v", err)
	}
	return resp, mdn
}

// captured returns the one message the instance stored, and its event.
func captured(t *testing.T, in *testutil.Instance) (*as2.Message, *event.Event) {
	t.Helper()
	evs := in.WaitForEvents(1, store.Query{Plugin: as2.Name}, 3*time.Second)
	if len(evs) != 1 {
		t.Fatalf("stored %d events, want 1", len(evs))
	}
	m, ok := as2.MessageOf(evs[0])
	if !ok {
		t.Fatal("event does not carry an as2.Message")
	}
	return m, evs[0]
}

// assertMDNEnvelope checks the parts of the reply that are the same whatever
// happened to the message: RFC 4130 §6.2's header swap, a 200, a real
// Content-Length (some AS2 clients will not read a chunked MDN), and the
// Original-Message-ID echoed exactly.
func assertMDNEnvelope(t *testing.T, resp *http.Response, mdn []byte) {
	t.Helper()
	assertMDNEnvelopeFor(t, resp, mdn, "TOMMY", "PARTNER")
}

// assertMDNEnvelopeFor is the same with the identifier swap spelled out, for
// the one case where the request did not address TOMMY.
func assertMDNEnvelopeFor(t *testing.T, resp *http.Response, mdn []byte, wantFrom, wantTo string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200\n%s", resp.StatusCode, mdn)
	}
	if got := resp.Header.Get("AS2-From"); got != wantFrom {
		t.Errorf("AS2-From = %q, want the request's AS2-To %q", got, wantFrom)
	}
	if got := resp.Header.Get("AS2-To"); got != wantTo {
		t.Errorf("AS2-To = %q, want the request's AS2-From %q", got, wantTo)
	}
	if got := resp.Header.Get("AS2-Version"); got != as2.Version {
		t.Errorf("AS2-Version = %q, want %q", got, as2.Version)
	}
	if resp.ContentLength != int64(len(mdn)) {
		t.Errorf("Content-Length = %d, body is %d bytes", resp.ContentLength, len(mdn))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/report") && !strings.HasPrefix(ct, "multipart/signed") {
		t.Errorf("Content-Type = %q, want a multipart/report or multipart/signed MDN", ct)
	}
	want := "Original-Message-ID: " + messageID + "\r\n"
	if !bytes.Contains(mdn, []byte(want)) {
		t.Errorf("MDN does not echo %q verbatim\n%s", want, mdn)
	}
}

// digest is what OpenSSL says the digest of these bytes is, base64 as it goes
// on the wire. Comparing tommy's Received-Content-MIC against this is the point
// of the whole exercise: the MIC is the most interop-sensitive number in AS2
// and a self-computed reference proves nothing.
func digest(t *testing.T, alg string, data []byte) string {
	t.Helper()
	raw := openssl(t, data, "dgst", "-"+alg, "-binary")
	return strings.TrimSpace(string(openssl(t, raw, "base64", "-A")))
}

// payloadBytes fetches the stored document back through the plugin's own API,
// so the assertion covers the whole path rather than the in-memory model.
func payloadBytes(t *testing.T, in *testutil.Instance, ev *event.Event) []byte {
	t.Helper()
	status, body := in.GetBody(in.API("/as2/messages/" + string(ev.ID) + "/payload"))
	if status != http.StatusOK {
		t.Fatalf("payload fetch = %d: %s", status, body)
	}
	return []byte(body)
}

func TestPlainMessage(t *testing.T) {
	in := start(t, nil)

	h := http.Header{}
	h.Set("Content-Type", "application/edi-x12")
	resp, mdn := post(t, in, h, []byte(sampleEDI))
	assertMDNEnvelope(t, resp, mdn)

	m, ev := captured(t, in)
	if m.Security.Signed || m.Security.Encrypted || m.Security.Compressed {
		t.Errorf("security = %+v, want unprotected", m.Security)
	}
	if m.From != "PARTNER" || m.To != "TOMMY" || m.MessageID != messageID {
		t.Errorf("identifiers = %q/%q/%q", m.From, m.To, m.MessageID)
	}
	if len(m.Issues) != 0 {
		t.Errorf("issues = %+v, want none", m.Issues)
	}
	if got := m.MDN.Disposition; got != as2.DispositionMode+"; "+as2.DispositionProcessed {
		t.Errorf("disposition = %q", got)
	}

	// RFC 4130 §7.4.3: an unsigned message's MIC is taken with SHA-1 and, per
	// §7.3.1, over the content alone with no MIME headers. Both halves are
	// asserted, because getting the coverage wrong is invisible until a real
	// partner disagrees.
	if m.MIC == nil {
		t.Fatal("no MIC")
	}
	if m.MIC.Algorithm != as2.DefaultMICAlgorithm {
		t.Errorf("MIC algorithm = %q, want %q", m.MIC.Algorithm, as2.DefaultMICAlgorithm)
	}
	if m.MIC.Coverage != as2.MICOverContentOnly {
		t.Errorf("MIC coverage = %q, want %q", m.MIC.Coverage, as2.MICOverContentOnly)
	}
	if want := digest(t, "sha1", []byte(sampleEDI)); m.MIC.Digest != want {
		t.Errorf("MIC = %q, openssl says %q", m.MIC.Digest, want)
	}

	if m.Payload.Format != as2.FormatX12 {
		t.Errorf("format = %q, want %q", m.Payload.Format, as2.FormatX12)
	}
	if got := payloadBytes(t, in, ev); string(got) != sampleEDI {
		t.Errorf("stored payload does not round-trip:\n%q", got)
	}
}

func TestSignedMessage(t *testing.T) {
	in := start(t, nil)

	entity := ediEntity(sampleEDI)
	h, body := splitSMIME(t, cmsSign(t, entity, "sha256"))
	resp, mdn := post(t, in, h, body)
	assertMDNEnvelope(t, resp, mdn)

	m, ev := captured(t, in)
	if !m.Security.Signed || m.Security.Signature == nil {
		t.Fatalf("security = %+v, want signed", m.Security)
	}
	sig := m.Security.Signature
	if !sig.Verified {
		t.Errorf("signature did not verify: %s", sig.Error)
	}
	// No partner certificate is configured here, so "intact" is all that can be
	// claimed and SignerMatched must stay false. A provider that reported
	// otherwise would be asserting something it cannot know.
	if sig.SignerMatched || sig.PartnerConfigured {
		t.Errorf("SignerMatched=%v PartnerConfigured=%v, want both false", sig.SignerMatched, sig.PartnerConfigured)
	}
	if sig.DigestAlgorithm != "sha256" {
		t.Errorf("digest algorithm = %q", sig.DigestAlgorithm)
	}

	// The MIC covers exactly the bytes that were signed - the inner entity,
	// headers included and untouched. generate.sh proves `openssl cms -verify`
	// extracts those bytes verbatim, so hashing the entity we handed OpenSSL is
	// the same thing a partner does.
	if m.MIC == nil || m.MIC.Coverage != as2.MICOverSignedContent {
		t.Fatalf("MIC = %+v, want coverage %q", m.MIC, as2.MICOverSignedContent)
	}
	if want := digest(t, "sha256", entity); m.MIC.Digest != want {
		t.Errorf("MIC = %q, openssl says %q", m.MIC.Digest, want)
	}
	if got := payloadBytes(t, in, ev); string(got) != sampleEDI {
		t.Errorf("stored payload does not round-trip:\n%q", got)
	}
}

func TestEncryptedMessage(t *testing.T) {
	in := start(t, nil)

	entity := ediEntity(sampleEDI)
	h := http.Header{}
	h.Set("Content-Type", "application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m")
	h.Set("Content-Transfer-Encoding", "base64")
	resp, mdn := post(t, in, h, cmsEncryptBase64(t, entity, key("tommy.crt")))
	assertMDNEnvelope(t, resp, mdn)

	m, ev := captured(t, in)
	if !m.Security.Encrypted || m.Security.Encryption == nil {
		t.Fatalf("security = %+v, want encrypted", m.Security)
	}
	if !m.Security.Encryption.Decrypted {
		t.Fatalf("did not decrypt: %s", m.Security.Encryption.Error)
	}
	if m.Security.Signed {
		t.Error("reported signed, but nothing signed this")
	}
	// RFC 4130 §7.3.1: for an encrypted, unsigned message the digest covers the
	// decrypted MIME entity with its headers - not the document alone.
	if m.MIC == nil || m.MIC.Coverage != as2.MICOverDecryptedEntity {
		t.Fatalf("MIC = %+v, want coverage %q", m.MIC, as2.MICOverDecryptedEntity)
	}
	if want := digest(t, "sha1", entity); m.MIC.Digest != want {
		t.Errorf("MIC = %q, openssl says %q", m.MIC.Digest, want)
	}
	if got := payloadBytes(t, in, ev); string(got) != sampleEDI {
		t.Errorf("stored payload does not round-trip:\n%q", got)
	}
}

func TestSignedThenEncryptedMessage(t *testing.T) {
	in := start(t, nil)

	entity := ediEntity(sampleEDI)
	// The complete signed S/MIME message, its own MIME headers included, is
	// what gets encrypted. Those headers are inside the ciphertext and are part
	// of what the receiver has to parse back out.
	signed := cmsSign(t, entity, "sha256")

	h := http.Header{}
	h.Set("Content-Type", "application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m")
	h.Set("Content-Transfer-Encoding", "base64")
	resp, mdn := post(t, in, signedReceipt(h), cmsEncryptBase64(t, signed, key("tommy.crt")))
	assertMDNEnvelope(t, resp, mdn)

	m, ev := captured(t, in)
	if !m.Security.Encrypted || !m.Security.Signed {
		t.Fatalf("security = %q, want both signed and encrypted", m.Security.Summary())
	}
	if !m.Security.Encryption.Decrypted {
		t.Fatalf("did not decrypt: %s", m.Security.Encryption.Error)
	}
	if !m.Security.Signature.Verified {
		t.Errorf("signature did not verify: %s", m.Security.Signature.Error)
	}
	if len(m.Issues) != 0 {
		t.Errorf("issues = %+v, want none", m.Issues)
	}
	// Signed wins over encrypted: the digest is over what was signed.
	if m.MIC == nil || m.MIC.Coverage != as2.MICOverSignedContent {
		t.Fatalf("MIC = %+v, want coverage %q", m.MIC, as2.MICOverSignedContent)
	}
	if want := digest(t, "sha256", entity); m.MIC.Digest != want {
		t.Errorf("MIC = %q, openssl says %q", m.MIC.Digest, want)
	}
	if got := payloadBytes(t, in, ev); string(got) != sampleEDI {
		t.Errorf("stored payload does not round-trip:\n%q", got)
	}

	// And the receipt for the most demanding case must be one OpenSSL accepts.
	report := verifyMDN(t, resp, mdn)
	if !bytes.Contains(report, []byte("Original-Message-ID: "+messageID)) {
		t.Errorf("verified MDN does not carry the message id:\n%s", report)
	}
}

// TestMDNVerifiesWithOpenSSL is the assertion that cannot be made any other
// way. A signed MDN that tommy's own code can parse but OpenSSL rejects is a
// broken MDN, and only an independent implementation can say so.
func TestMDNVerifiesWithOpenSSL(t *testing.T) {
	in := start(t, nil)

	h, body := splitSMIME(t, cmsSign(t, ediEntity(sampleEDI), "sha256"))
	resp, mdn := post(t, in, signedReceipt(h), body)
	assertMDNEnvelope(t, resp, mdn)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/signed") {
		t.Fatalf("Content-Type = %q, want multipart/signed - a signed receipt was requested", ct)
	}
	m, _ := captured(t, in)
	if m.MDN == nil || !m.MDN.Signed {
		t.Fatalf("MDN record does not say it was signed: %+v", m.MDN)
	}

	report := verifyMDN(t, resp, mdn)
	for _, want := range []string{
		"Original-Message-ID: " + messageID,
		"Disposition: " + as2.DispositionMode + "; " + as2.DispositionProcessed,
		"Received-Content-MIC: " + m.MIC.Header(),
	} {
		if !bytes.Contains(report, []byte(want)) {
			t.Errorf("verified MDN is missing %q:\n%s", want, report)
		}
	}
}

// verifyMDN hands the receipt to OpenSSL and returns the content it was willing
// to vouch for.
//
// The reassembly step is the interesting part: an AS2 MDN's multipart boundary
// lives in the HTTP Content-Type header, not in the body, so the reply is only
// a MIME message once the two are put back together. -purpose any because the
// certificate is self-signed and exists to be trusted by exactly one partner.
func verifyMDN(t *testing.T, resp *http.Response, mdn []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mdn.mime")
	entity := append([]byte("Content-Type: "+resp.Header.Get("Content-Type")+"\r\n\r\n"), mdn...)
	if err := os.WriteFile(path, entity, 0o600); err != nil {
		t.Fatalf("write MDN: %v", err)
	}
	return openssl(t, nil, "smime", "-verify", "-in", path,
		"-CAfile", key("tommy.crt"), "-purpose", "any")
}
