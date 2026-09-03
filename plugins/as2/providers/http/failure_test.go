package http_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// Nothing here is refused, and that is the whole point of these tests.
//
// RFC 4130 §7.4.4 reserves the "failed" disposition-type for being unable to
// produce an MDN at all; a message that would not decrypt or whose signature is
// wrong is "processed" with an error modifier (§7.5.4). So every case below
// asserts a 200 with a real receipt saying honestly what went wrong - a fake
// that answers 400 teaches its user nothing and, worse, stops their software
// ever exercising the path they were trying to test.

// assertProcessedError checks the shape every recoverable failure has to have:
// a real MDN, on a 200, carrying the modifier the RFC names for that fault.
func assertProcessedError(t *testing.T, resp *http.Response, mdn []byte, modifier string) {
	t.Helper()
	assertMDNEnvelope(t, resp, mdn)
	want := "processed/Error: " + modifier
	if !bytes.Contains(mdn, []byte(want)) {
		t.Errorf("MDN does not report %q\n%s", want, mdn)
	}
	if bytes.Contains(mdn, []byte("; failed/")) {
		t.Errorf("MDN used the failed disposition-type, which RFC 4130 §7.4.4 reserves "+
			"for being unable to produce an MDN at all\n%s", mdn)
	}
}

// TestCorruptSignature alters the signed content in flight. The signature is
// structurally perfect and cryptographically wrong, which RFC 4130 §7.5.4 calls
// integrity-check-failed: the receiver could not verify content integrity, as
// distinct from not being able to tell who sent it.
func TestCorruptSignature(t *testing.T) {
	in := start(t, nil)

	h, body := splitSMIME(t, cmsSign(t, ediEntity(sampleEDI), "sha256"))
	tampered := bytes.Replace(body, []byte("WIDGET-1"), []byte("WIDGET-2"), 1)
	if bytes.Equal(tampered, body) {
		t.Fatal("the token to tamper with is not in the signed body")
	}

	resp, mdn := post(t, in, h, tampered)
	assertProcessedError(t, resp, mdn, "integrity-check-failed")

	m, _ := captured(t, in)
	if !m.HasIssue(issueIntegrityCheckFailed) {
		t.Errorf("issues = %+v, want %s", m.Issues, issueIntegrityCheckFailed)
	}
	if m.Security.Signature == nil || m.Security.Signature.Verified {
		t.Errorf("signature reported as verified over content that was altered")
	}
	// The altered document is still stored. A capture tool that discards what
	// it could not vouch for is exactly the tool nobody can debug with.
	if m.Payload.Size == 0 {
		t.Error("nothing was stored for a message whose signature failed")
	}
}

// TestUndecryptableContent encrypts to a certificate whose private key nothing
// here holds - the commonest real AS2 failure, and the one whose symptom is
// otherwise a silent hang.
func TestUndecryptableContent(t *testing.T) {
	in := start(t, nil)

	h := http.Header{}
	h.Set("Content-Type", "application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m")
	h.Set("Content-Transfer-Encoding", "base64")
	resp, mdn := post(t, in, h, cmsEncryptBase64(t, ediEntity(sampleEDI), key("stranger.crt")))
	assertProcessedError(t, resp, mdn, "decryption-failed")

	m, _ := captured(t, in)
	if !m.HasIssue(issueDecryptionFailed) {
		t.Errorf("issues = %+v, want %s", m.Issues, issueDecryptionFailed)
	}
	// The ciphertext becomes the payload, flagged as not being the business
	// document. Keeping it is what lets somebody diff the recipient serial
	// against the certificate they meant to use.
	if !m.Payload.Recovered {
		t.Error("payload is not marked Recovered, so the UI would present ciphertext as EDI")
	}
	if m.Security.Encryption == nil || len(m.Security.Encryption.Recipients) == 0 {
		t.Error("no recipients recorded, so there is no way to see who it was encrypted to")
	}
}

// TestMalformedMIME declares a multipart whose boundary appears nowhere. There
// is no RFC-defined modifier for "your MIME is broken", so it lands on
// unexpected-processing-error, which is what that modifier exists for.
func TestMalformedMIME(t *testing.T) {
	in := start(t, nil)

	h := http.Header{}
	h.Set("Content-Type", `multipart/signed; protocol="application/pkcs7-signature"; micalg=sha-256; boundary="nowhere"`)
	resp, mdn := post(t, in, h, []byte("this body contains no delimiter at all\r\n"))
	assertMDNEnvelope(t, resp, mdn)

	m, _ := captured(t, in)
	if !m.HasIssue(issueMalformedMIME) {
		t.Errorf("issues = %+v, want %s", m.Issues, issueMalformedMIME)
	}
	if strings.Contains(m.MDN.Disposition, "; failed/") {
		t.Errorf("disposition = %q, want processed", m.MDN.Disposition)
	}
}

// TestEmptyBody is the probe case: something opened a connection and sent
// nothing. It must still get a receipt rather than a stack trace.
func TestEmptyBody(t *testing.T) {
	in := start(t, nil)

	h := http.Header{}
	h.Set("Content-Type", "application/edi-x12")
	resp, mdn := post(t, in, h, nil)
	assertMDNEnvelope(t, resp, mdn)

	m, _ := captured(t, in)
	if !m.HasIssue(issueEmptyBody) {
		t.Errorf("issues = %+v, want %s", m.Issues, issueEmptyBody)
	}
}

// TestMaxBody covers the one setting that can refuse a request, and it refuses
// at the HTTP level on purpose: a body too large to read was never a message,
// so there is nothing to build an MDN about.
func TestMaxBody(t *testing.T) {
	in := start(t, map[string]any{"max_body": 512})

	h := http.Header{}
	h.Set("Content-Type", "application/edi-x12")
	resp, body := post(t, in, h, bytes.Repeat([]byte("X"), 4096))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413\n%s", resp.StatusCode, body)
	}

	// And a message under the cap still goes through, so the setting is a cap
	// rather than an off switch.
	resp, mdn := post(t, in, h, []byte(sampleEDI))
	assertMDNEnvelope(t, resp, mdn)
}
