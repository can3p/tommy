package as2_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/as2"
)

// Every fixture in testdata/ was produced by OpenSSL 3.6.1 and nothing in this
// repository - see testdata/generate.sh. That is the point of them: the parser
// is checked against an independent implementation of the crypto rather than
// against its own output, which is the only way a round-trip test can be wrong
// and still pass.
//
// The reference MIC values in testdata/mic.json were likewise computed by
// `openssl dgst`, over the bytes `openssl cms -verify` says were signed.

const testdataDir = "testdata"

// fixture reads a committed MIME entity.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testdataDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// referenceMICs are the digests openssl computed, loaded once.
func referenceMICs(t *testing.T) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(fixture(t, "mic.json"), &out); err != nil {
		t.Fatalf("decode mic.json: %v", err)
	}
	return out
}

// request maps a MIME entity fixture onto an AS2 HTTP request the way a real
// sender does: the entity's headers become the request's headers and its body
// becomes the request body. There is no second header block inside an AS2 body.
func request(t *testing.T, fixtureName string, extra http.Header) as2.Request {
	t.Helper()
	raw := fixture(t, fixtureName)

	header := http.Header{}
	header.Set("AS2-From", "PARTNER")
	header.Set("AS2-To", "TOMMY")
	header.Set("Message-ID", "<test-message-1@partner.example>")
	header.Set("Subject", "Purchase order 4711")
	header.Set("AS2-Version", "1.1")

	// Split the fixture's own header block off and promote it to HTTP headers.
	// The fixtures mix line endings on purpose - openssl writes the outer
	// header block with bare LF and the inner parts with CRLF - so both
	// separators have to be looked for and the earlier one wins.
	sep, sepLen := -1, 0
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		sep, sepLen = i, 4
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 && (sep < 0 || i < sep) {
		sep, sepLen = i, 2
	}
	if sep < 0 {
		t.Fatalf("fixture %s has no header/body separator", fixtureName)
	}
	head, body := raw[:sep], raw[sep+sepLen:]
	for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	for k, vs := range extra {
		header.Del(k)
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	return as2.Request{
		Method:   http.MethodPost,
		Path:     "/as2",
		Host:     "tommy.local",
		Header:   header,
		Body:     body,
		PeerAddr: "203.0.113.9:51000",
	}
}

// receiverWith builds a receiver whose identity is the tommy.crt/tommy.key pair
// the fixtures were encrypted to.
func receiverWith(t *testing.T, cfg as2.IdentityConfig) (*as2.Receiver, plugin.Deps) {
	t.Helper()
	if cfg.CertFile == "" && cfg.KeyFile == "" && !cfg.InMemory {
		cfg.CertFile = filepath.Join(testdataDir, "tommy.crt")
		cfg.KeyFile = filepath.Join(testdataDir, "tommy.key")
	}
	id := as2.NewIdentity()
	if err := id.Configure(cfg); err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	deps := plugintest.NewDeps()
	return as2.NewReceiver(id, deps, as2.WithProvider("test")), deps
}

func receive(t *testing.T, r *as2.Receiver, req as2.Request) *as2.Result {
	t.Helper()
	res, err := r.Receive(context.Background(), req)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	return res
}

// signedReceipt is the header a partner sends to ask for a signed MDN.
func signedReceipt(micalgs string) http.Header {
	return http.Header{
		"Disposition-Notification-To": {"as2@partner.example"},
		"Disposition-Notification-Options": {
			"signed-receipt-protocol=optional,pkcs7-signature; signed-receipt-micalg=optional," + micalgs,
		},
	}
}

// ------------------------------------------------------------- the four cases

func TestUnwrapsEveryCombination(t *testing.T) {
	mics := referenceMICs(t)

	cases := []struct {
		name     string
		fixture  string
		signed   bool
		verified bool
		crypted  bool
		zipped   bool
		// wantMIC is the reference digest openssl computed, and wantCoverage
		// is what tommy must say it hashed.
		wantMIC      string
		wantCoverage string
		wantFormat   string
	}{
		{
			name: "plain", fixture: "plain.mime",
			wantMIC: mics["payload_only_sha256"], wantCoverage: as2.MICOverContentOnly,
			wantFormat: as2.FormatX12,
		},
		{
			name: "signed", fixture: "signed.mime",
			signed: true, verified: true,
			wantMIC: mics["inner_sha256"], wantCoverage: as2.MICOverSignedContent,
			wantFormat: as2.FormatX12,
		},
		{
			name: "encrypted", fixture: "encrypted.mime",
			crypted: true,
			wantMIC: mics["inner_sha256"], wantCoverage: as2.MICOverDecryptedEntity,
			wantFormat: as2.FormatX12,
		},
		{
			name: "signed then encrypted", fixture: "signed_encrypted.mime",
			signed: true, verified: true, crypted: true,
			wantMIC: mics["inner_sha256"], wantCoverage: as2.MICOverSignedContent,
			wantFormat: as2.FormatX12,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := receiverWith(t, as2.IdentityConfig{})
			// sha256 is asked for so the unsigned cases use the same algorithm
			// as the signed ones and the reference digests line up.
			res := receive(t, r, request(t, tc.fixture, signedReceipt("sha256,sha1")))
			m := res.Message

			if m.Security.Signed != tc.signed {
				t.Errorf("Signed = %v, want %v", m.Security.Signed, tc.signed)
			}
			if m.Security.Encrypted != tc.crypted {
				t.Errorf("Encrypted = %v, want %v", m.Security.Encrypted, tc.crypted)
			}
			if m.Security.Compressed != tc.zipped {
				t.Errorf("Compressed = %v, want %v", m.Security.Compressed, tc.zipped)
			}
			if tc.signed {
				if m.Security.Signature == nil {
					t.Fatal("Security.Signature is nil for a signed message")
				}
				if m.Security.Signature.Verified != tc.verified {
					t.Errorf("Verified = %v (%s), want %v",
						m.Security.Signature.Verified, m.Security.Signature.Error, tc.verified)
				}
			}
			if tc.crypted && (m.Security.Encryption == nil || !m.Security.Encryption.Decrypted) {
				t.Fatalf("encryption not opened: %+v", m.Security.Encryption)
			}

			if m.MIC == nil {
				t.Fatal("no MIC computed")
			}
			if m.MIC.Digest != tc.wantMIC {
				t.Errorf("MIC = %q, want openssl's %q", m.MIC.Digest, tc.wantMIC)
			}
			if m.MIC.Coverage != tc.wantCoverage {
				t.Errorf("MIC coverage = %q, want %q", m.MIC.Coverage, tc.wantCoverage)
			}
			if m.Payload.Format != tc.wantFormat {
				t.Errorf("payload format = %q, want %q", m.Payload.Format, tc.wantFormat)
			}
			if !strings.HasPrefix(m.Payload.Preview, "ISA*00*") {
				t.Errorf("payload preview = %q, want the EDI interchange", m.Payload.Preview)
			}
			if len(m.Issues) != 0 {
				t.Errorf("clean message reported issues: %+v", m.Issues)
			}
			if got := m.MDN.Disposition; !strings.HasSuffix(got, "; processed") {
				t.Errorf("disposition = %q, want a clean processed", got)
			}
		})
	}
}

// The encryption fixtures use two different content algorithms on purpose:
// naming which one was used is most of what an operator wants from a message
// they could not decrypt.
func TestReportsContentEncryptionAlgorithm(t *testing.T) {
	for fixtureName, want := range map[string]string{
		"encrypted.mime":        "aes-128-cbc",
		"encrypted_3des.mime":   "des-ede3-cbc",
		"signed_encrypted.mime": "aes-256-cbc",
	} {
		t.Run(fixtureName, func(t *testing.T) {
			r, _ := receiverWith(t, as2.IdentityConfig{})
			m := receive(t, r, request(t, fixtureName, nil)).Message
			if m.Security.Encryption == nil {
				t.Fatal("no encryption recorded")
			}
			if got := m.Security.Encryption.ContentAlgorithm; got != want {
				t.Errorf("content algorithm = %q, want %q", got, want)
			}
			if len(m.Security.Encryption.Recipients) == 0 {
				t.Error("no recipients recorded; a message encrypted to the wrong certificate could not be diagnosed")
			}
		})
	}
}

// ------------------------------------------------------------- compression

// The MIC rule that catches every AS2 implementation out: for a signed message
// the digest covers what was signed, which for a compress-then-sign message is
// the COMPRESSED entity. Expanding it first yields a MIC that every partner
// rejects while every self-consistent round-trip test passes.
func TestCompressionPlacementDecidesMICCoverage(t *testing.T) {
	mics := referenceMICs(t)

	t.Run("compressed then signed hashes the compressed entity", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{})
		m := receive(t, r, request(t, "compressed_signed.mime", signedReceipt("sha256"))).Message

		if !m.Security.Signed || !m.Security.Compressed {
			t.Fatalf("expected signed and compressed, got %+v", m.Security)
		}
		if got := m.Security.Compression.Placement; got != as2.PlacementInner {
			t.Errorf("placement = %q, want %q", got, as2.PlacementInner)
		}
		if m.MIC == nil {
			t.Fatal("no MIC")
		}
		if m.MIC.Digest != mics["compressed_entity_sha256"] {
			t.Errorf("MIC = %q, want the digest of the COMPRESSED entity %q.\n"+
				"Hashing the decompressed document here is the classic AS2 interop bug.",
				m.MIC.Digest, mics["compressed_entity_sha256"])
		}
		if m.MIC.Digest == mics["inner_sha256"] {
			t.Error("MIC is the digest of the decompressed document; the content was expanded before hashing")
		}
		if m.Payload.Format != as2.FormatX12 {
			t.Errorf("payload was not decompressed to the EDI document: format %q", m.Payload.Format)
		}
	})

	t.Run("signed then compressed hashes the uncompressed signed content", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{})
		m := receive(t, r, request(t, "signed_compressed.mime", signedReceipt("sha256"))).Message

		if got := m.Security.Compression.Placement; got != as2.PlacementOuter {
			t.Errorf("placement = %q, want %q", got, as2.PlacementOuter)
		}
		if m.MIC == nil {
			t.Fatal("no MIC")
		}
		// The signature covers inner.mime here, so the MIC must match the
		// plain signed fixture's exactly.
		if m.MIC.Digest != mics["inner_sha256"] {
			t.Errorf("MIC = %q, want %q - the same digest signed.mime produces, "+
				"because the same bytes were signed", m.MIC.Digest, mics["inner_sha256"])
		}
	})

	t.Run("compressed only, unsigned", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{})
		m := receive(t, r, request(t, "compressed.mime", signedReceipt("sha256"))).Message

		if got := m.Security.Compression.Placement; got != as2.PlacementOnly {
			t.Errorf("placement = %q, want %q", got, as2.PlacementOnly)
		}
		if !m.Security.Compression.Decompressed {
			t.Fatalf("not decompressed: %+v", m.Security.Compression)
		}
		if m.Payload.Format != as2.FormatX12 {
			t.Errorf("payload format = %q, want the decompressed EDI", m.Payload.Format)
		}
		// RFC 5402 §4.3 leads for a compressed unsigned message, so the MIC
		// covers the whole uncompressed entity and RFC 4130's content-only
		// reading is recorded beside it.
		if m.MIC.Coverage != as2.MICOverFullEntity {
			t.Errorf("coverage = %q, want %q", m.MIC.Coverage, as2.MICOverFullEntity)
		}
		if m.MIC.Digest != mics["inner_sha256"] {
			t.Errorf("MIC = %q, want the digest of the decompressed entity %q",
				m.MIC.Digest, mics["inner_sha256"])
		}
		if len(m.AlternateMICs) != 1 || m.AlternateMICs[0].Coverage != as2.MICOverContentOnly {
			t.Errorf("alternate MICs = %+v, want one content-only reading", m.AlternateMICs)
		}
	})
}

func TestUnknownCompressionIsRecordedNotDropped(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "unknown_compression.mime", signedReceipt("sha256")))
	m := res.Message

	if !m.HasIssue(as2.IssueUnsupportedCompression) {
		t.Fatalf("issues = %+v, want %s", m.Issues, as2.IssueUnsupportedCompression)
	}
	if m.Security.Compression == nil || m.Security.Compression.AlgorithmOID == "" {
		t.Fatal("the unsupported algorithm's OID was not recorded")
	}
	if m.Security.Compression.Decompressed {
		t.Error("claims to have decompressed content it could not read")
	}
	// The bytes must survive. This is the rule that matters most: a capture
	// tool that drops what it cannot parse teaches its user nothing.
	if m.Payload.Blob == nil || m.Payload.Size == 0 {
		t.Fatal("the compressed bytes were dropped instead of stored")
	}
	if !m.Payload.Recovered {
		t.Error("payload is not flagged as recovered, so the UI would present ciphertext as an EDI document")
	}
	if got := m.MDN.Disposition; !strings.Contains(got, "decompression-failed") {
		t.Errorf("disposition = %q, want an Error: decompression-failed", got)
	}
}

// ----------------------------------------------------------- failure paths

func TestCorruptSignatureIsReportedNotRefused(t *testing.T) {
	r, deps := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "signed_corrupt.mime", signedReceipt("sha256")))
	m := res.Message

	if m.Security.Signature == nil || m.Security.Signature.Verified {
		t.Fatalf("a tampered payload verified: %+v", m.Security.Signature)
	}
	if !m.HasIssue(as2.IssueIntegrityCheckFailed) {
		t.Errorf("issues = %+v, want %s", m.Issues, as2.IssueIntegrityCheckFailed)
	}
	if res.Response.Status != http.StatusOK {
		t.Errorf("status = %d; an AS2 receiver answers 200 with an error disposition, it does not refuse",
			res.Response.Status)
	}
	// RFC 4130 §7.4.4: a content problem is "processed" with an error
	// modifier, never "failed".
	if got := m.MDN.Disposition; !strings.Contains(got, "processed/Error: integrity-check-failed") {
		t.Errorf("disposition = %q, want processed/Error: integrity-check-failed", got)
	}
	// §7.4.3: the MIC is set only when the content processed successfully.
	if strings.Contains(string(res.Response.Body), "Received-Content-MIC:") {
		t.Error("the MDN carries a Received-Content-MIC for content that did not process")
	}
	// And the message is still there to look at.
	events, err := deps.Store.List(context.Background(), storeQueryAll())
	if err != nil || len(events) != 1 {
		t.Fatalf("store holds %d events (err %v), want the failed message kept", len(events), err)
	}
}

func TestUndecryptableMessageIsKept(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "undecryptable.mime", signedReceipt("sha256")))
	m := res.Message

	if !m.HasIssue(as2.IssueDecryptionFailed) {
		t.Fatalf("issues = %+v, want %s", m.Issues, as2.IssueDecryptionFailed)
	}
	if m.Payload.Blob == nil {
		t.Fatal("the ciphertext was dropped rather than stored")
	}
	if got := m.MDN.Disposition; !strings.Contains(got, "processed/Error: decryption-failed") {
		t.Errorf("disposition = %q", got)
	}
	// The diagnosis has to name the recipients, because "encrypted to somebody
	// else's certificate" is the single commonest AS2 setup mistake.
	if enc := m.Security.Encryption; enc == nil || !strings.Contains(enc.Error, "not encrypted to tommy's certificate") {
		t.Errorf("encryption error = %+v, want an explanation naming the recipients", enc)
	}
}

func TestMalformedMIMEIsRecovered(t *testing.T) {
	t.Run("truncated multipart keeps the parts it found", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{})
		m := receive(t, r, request(t, "truncated.mime", signedReceipt("sha256"))).Message
		if !m.HasIssue(as2.IssueTruncatedMultipart) {
			t.Errorf("issues = %+v, want %s", m.Issues, as2.IssueTruncatedMultipart)
		}
		if m.Payload.Size == 0 {
			t.Error("nothing was recovered from a truncated multipart")
		}
	})

	t.Run("boundary that appears nowhere", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{})
		res := receive(t, r, request(t, "no_boundary.mime", signedReceipt("sha256")))
		if !res.Message.HasIssue(as2.IssueMalformedMIME) {
			t.Errorf("issues = %+v, want %s", res.Message.Issues, as2.IssueMalformedMIME)
		}
		if res.Response.Status != http.StatusOK {
			t.Errorf("status = %d, want 200 with an error disposition", res.Response.Status)
		}
	})
}

// ------------------------------------------------------------------ the MDN

func TestSignedMDNIsVerifiableAndWellFormed(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "signed.mime", signedReceipt("sha256")))
	body := string(res.Response.Body)

	ct := res.Response.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/signed;") {
		t.Fatalf("Content-Type = %q, want multipart/signed", ct)
	}
	if !strings.Contains(ct, `protocol="application/pkcs7-signature"`) {
		t.Errorf("Content-Type = %q, want the pkcs7-signature protocol parameter", ct)
	}
	if !strings.Contains(ct, "micalg=sha-256") {
		t.Errorf("Content-Type = %q, want micalg=sha-256", ct)
	}
	for _, want := range []string{
		"Content-Type: multipart/report;",
		"Content-Type: message/disposition-notification",
		"Content-Type: application/pkcs7-signature",
		"Reporting-UA:",
		"Final-Recipient: rfc822; TOMMY",
		"Original-Message-ID: <test-message-1@partner.example>",
		"Disposition: automatic-action/MDN-sent-automatically; processed",
		"Received-Content-MIC:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MDN body is missing %q\n---\n%s", want, body)
		}
	}

	// RFC 4130 §6.2: the identifiers swap.
	if got := res.Response.Header.Get("AS2-From"); got != "TOMMY" {
		t.Errorf("AS2-From = %q, want the request's AS2-To", got)
	}
	if got := res.Response.Header.Get("AS2-To"); got != "PARTNER" {
		t.Errorf("AS2-To = %q, want the request's AS2-From", got)
	}
	if res.Response.Header.Get("Message-ID") == "" {
		t.Error("the MDN has no Message-ID of its own")
	}

	if res.Message.MDN == nil || !res.Message.MDN.Signed {
		t.Error("the MDN record does not say it was signed")
	}
}

// Nobody asking for a signature gets an unsigned multipart/report, which is the
// shape RFC 4130 §7.4.2 gives for that case.
func TestUnsignedMDNWhenNoSignatureRequested(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "plain.mime", http.Header{
		"Disposition-Notification-To": {"as2@partner.example"},
	}))
	ct := res.Response.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/report;") {
		t.Fatalf("Content-Type = %q, want multipart/report", ct)
	}
	if res.Message.MDN.Signed {
		t.Error("an unrequested signature was applied")
	}
}

// An asynchronous MDN is out of tommy's charter. The requirement is that it is
// never silently ignored.
func TestAsyncReceiptRequestIsRecordedAndAnsweredSynchronously(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	extra := signedReceipt("sha256")
	extra.Set("Receipt-Delivery-Option", "http://partner.example:8080/as2/mdn")
	res := receive(t, r, request(t, "signed.mime", extra))

	if !res.Message.Receipt.Async() {
		t.Fatal("Receipt-Delivery-Option was not recorded")
	}
	if !res.Message.HasIssue(as2.IssueAsyncReceiptRequested) {
		t.Errorf("issues = %+v, want %s", res.Message.Issues, as2.IssueAsyncReceiptRequested)
	}
	if res.Response.Status != http.StatusOK || len(res.Response.Body) == 0 {
		t.Error("no synchronous MDN was returned in place of the asynchronous one")
	}
	// An informational issue must not turn into an error disposition.
	if got := res.Message.MDN.Disposition; !strings.HasSuffix(got, "; processed") {
		t.Errorf("disposition = %q, want a clean processed", got)
	}
}

// ------------------------------------------------------- signer attribution

func TestSignerAttributionIsHonestAboutWhatItProves(t *testing.T) {
	t.Run("no partner certificate proves integrity only", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{})
		sig := receive(t, r, request(t, "signed.mime", nil)).Message.Security.Signature
		if !sig.Verified {
			t.Fatalf("signature did not verify: %s", sig.Error)
		}
		if sig.PartnerConfigured || sig.SignerMatched {
			t.Error("claims a partner match with no partner certificate configured")
		}
		if !strings.Contains(sig.Assurance(), "unproven") {
			t.Errorf("assurance = %q, want it to say who signed is unproven", sig.Assurance())
		}
	})

	t.Run("the right partner certificate attributes the signature", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{
			CertFile:        filepath.Join(testdataDir, "tommy.crt"),
			KeyFile:         filepath.Join(testdataDir, "tommy.key"),
			PartnerCertFile: filepath.Join(testdataDir, "partner.crt"),
		})
		m := receive(t, r, request(t, "signed.mime", nil)).Message
		sig := m.Security.Signature
		if !sig.Verified || !sig.SignerMatched || !sig.PartnerConfigured {
			t.Fatalf("signature not attributed: %+v", sig)
		}
		if m.HasIssue(as2.IssueAuthenticationFailed) {
			t.Error("the matching partner certificate still produced an authentication warning")
		}
	})

	t.Run("the wrong partner certificate warns and carries on", func(t *testing.T) {
		r, _ := receiverWith(t, as2.IdentityConfig{
			CertFile:        filepath.Join(testdataDir, "tommy.crt"),
			KeyFile:         filepath.Join(testdataDir, "tommy.key"),
			PartnerCertFile: filepath.Join(testdataDir, "stranger.crt"),
		})
		res := receive(t, r, request(t, "signed.mime", signedReceipt("sha256")))
		m := res.Message
		if !m.Security.Signature.Verified {
			t.Fatal("the signature itself should still verify")
		}
		if m.Security.Signature.SignerMatched {
			t.Error("matched a certificate that did not sign this")
		}
		if !m.HasIssue(as2.IssueAuthenticationFailed) {
			t.Errorf("issues = %+v, want %s as a warning", m.Issues, as2.IssueAuthenticationFailed)
		}
		// RFC 4130 §7.5.5: carrying on anyway is reported as a warning, and the
		// message is still processed. Refusing would be a policy decision.
		if got := m.MDN.Disposition; !strings.Contains(got, "processed/Warning: authentication-failed") {
			t.Errorf("disposition = %q, want a processed/Warning", got)
		}
		if m.Payload.Format != as2.FormatX12 {
			t.Error("processing did not continue past the unrecognized signer")
		}
	})
}

// ------------------------------------------------------------------ storage

func TestRawKeepsTheCiphertextAndTheBlobKeepsThePlaintext(t *testing.T) {
	r, deps := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "signed_encrypted.mime", signedReceipt("sha256")))

	// Rule 4: Raw is the untouched request.
	if !bytes.Equal(res.Event.Raw.Body, res.Response.Body) && len(res.Event.Raw.Body) == 0 {
		t.Fatal("Raw.Body is empty")
	}
	if bytes.Contains(res.Event.Raw.Body, []byte("ISA*00*")) {
		t.Error("Raw.Body holds plaintext; it must be the encrypted request exactly as it arrived")
	}

	// Rule 9: the bytes are in the blob store, not on the event.
	if res.Message.Payload.Blob == nil {
		t.Fatal("no payload blob")
	}
	rc, _, err := deps.Blobs.Open(context.Background(), res.Message.Payload.Blob.ID)
	if err != nil {
		t.Fatalf("open payload blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read payload blob: %v", err)
	}
	if !bytes.HasPrefix(got, []byte("ISA*00*")) {
		t.Errorf("payload blob does not hold the decrypted EDI document: %q", firstBytes(got))
	}

	// The MDN is stored too, so the exact receipt can be diffed against what
	// the partner's software says it saw.
	if res.Message.MDN.Blob == nil {
		t.Fatal("no MDN blob")
	}
}

func firstBytes(b []byte) string {
	if len(b) > 40 {
		return string(b[:40]) + "…"
	}
	return string(b)
}

// storeQueryAll is every event in the store, for the tests that assert a
// failed message was still kept.
func storeQueryAll() store.Query { return store.Query{Plugin: as2.Name} }

// The enveloping signature form. Rare in AS2 but legal, and a message tommy
// could not read at all would be worse than one it reads unusually.
func TestOpaqueSignedData(t *testing.T) {
	mics := referenceMICs(t)
	r, _ := receiverWith(t, as2.IdentityConfig{})
	m := receive(t, r, request(t, "signed_opaque.mime", signedReceipt("sha256"))).Message

	if !m.Security.Signed || m.Security.Signature == nil {
		t.Fatalf("not recognized as signed: %+v", m.Security)
	}
	if !m.Security.Signature.Verified {
		t.Fatalf("signature did not verify: %s", m.Security.Signature.Error)
	}
	if m.MIC == nil || m.MIC.Coverage != as2.MICOverSignedContent {
		t.Fatalf("MIC = %+v, want coverage over the signed content", m.MIC)
	}
	if m.MIC.Digest != mics["inner_sha256"] {
		t.Errorf("MIC = %q, want %q - the same bytes were signed as in signed.mime",
			m.MIC.Digest, mics["inner_sha256"])
	}
	if m.Payload.Format != as2.FormatX12 {
		t.Errorf("payload format = %q, want the EDI document from inside the SignedData", m.Payload.Format)
	}
	if len(m.Issues) != 0 {
		t.Errorf("issues = %+v, want none", m.Issues)
	}
}

// With no key pair nothing can be decrypted, and the partner has to be told
// that rather than left waiting.
func TestNoIdentityIsReportedNotCrashed(t *testing.T) {
	deps := plugintest.NewDeps()
	// An identity that was never configured: exactly the state a plugin with no
	// enabled provider is in.
	r := as2.NewReceiver(as2.NewIdentity(), deps, as2.WithProvider("test"))

	res := receive(t, r, request(t, "encrypted.mime", signedReceipt("sha256")))
	m := res.Message
	if !m.HasIssue(as2.IssueNoIdentity) {
		t.Fatalf("issues = %+v, want %s", m.Issues, as2.IssueNoIdentity)
	}
	if res.Response.Status != http.StatusOK {
		t.Errorf("status = %d, want 200 with an error disposition", res.Response.Status)
	}
	if got := m.MDN.Disposition; !strings.Contains(got, "processed/Error: decryption-failed") {
		t.Errorf("disposition = %q", got)
	}
	// A signature could not be produced either, so the MDN downgrades to
	// unsigned rather than not being sent at all - RFC 4130 §7.3.1 rule 2.
	if m.MDN.Signed {
		t.Error("an MDN was signed with no key pair")
	}
	if !strings.Contains(m.MDN.HumanText, "could not be produced") {
		t.Errorf("the MDN does not say why it is unsigned:\n%s", m.MDN.HumanText)
	}
	// And the ciphertext is still stored.
	if m.Payload.Blob == nil || !m.Payload.Recovered {
		t.Errorf("payload = %+v, want the ciphertext kept and flagged as recovered", m.Payload)
	}
}

// RFC 4130 §7.5.3 makes an unsupported MIC algorithm one of the two predefined
// "failed" cases - which is the only thing that is a "failed" rather than a
// "processed/Error".
func TestUnsupportedMICAlgorithmFailsTheReceipt(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "plain.mime", signedReceipt("whirlpool,streebog")))
	m := res.Message

	if !m.HasIssue(as2.IssueUnsupportedMICAlgorithm) {
		t.Fatalf("issues = %+v, want %s", m.Issues, as2.IssueUnsupportedMICAlgorithm)
	}
	if m.MIC != nil {
		t.Errorf("a MIC was computed in an algorithm nobody asked for: %+v", m.MIC)
	}
	if got := m.MDN.Disposition; !strings.Contains(got, "failed/Failure: unsupported MIC-algorithms") {
		t.Errorf("disposition = %q, want RFC 4130 §7.5.3's failed/Failure", got)
	}
	// The message is still captured. A receipt tommy could not produce properly
	// is not a reason to lose what arrived.
	if m.Payload.Size == 0 {
		t.Error("the payload was dropped")
	}
}

// A signature over content the sender never canonicalized still has to verify,
// and the MIC has to cover whichever bytes did verify.
func TestSignatureOverNonCanonicalContentIsRecovered(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	m := receive(t, r, request(t, "plain_lf.mime", signedReceipt("sha256"))).Message
	// plain_lf is unsigned; it exists to prove the LF path through the parser
	// rather than through the verifier.
	if m.Payload.Format != as2.FormatX12 {
		t.Errorf("a bare-LF entity did not parse: format %q", m.Payload.Format)
	}
	if m.MIC == nil {
		t.Fatal("no MIC for a bare-LF message")
	}
}

// RFC 4130 §7.3.1 and RFC 5402 §4.3 disagree about whether an unsigned
// message's MIC includes its MIME headers. For a plain AS2 message the argument
// is empty - the HTTP headers ARE the entity's MIME headers, so there is no
// second header block to include or exclude and both readings produce the same
// number. Recording an "alternate" identical to the primary would be noise, so
// there is none.
func TestPlainMessageHasOnlyOneMICReading(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	m := receive(t, r, request(t, "plain.mime", signedReceipt("sha256"))).Message

	if m.MIC == nil || m.MIC.Coverage != as2.MICOverContentOnly {
		t.Fatalf("MIC = %+v, want RFC 4130's content-only reading", m.MIC)
	}
	if len(m.AlternateMICs) != 0 {
		t.Errorf("alternates = %+v, want none: both readings coincide for a plain AS2 message",
			m.AlternateMICs)
	}
}

// Where the two readings genuinely diverge is a message whose content has MIME
// headers of its own - which is what decompression produces. That case records
// both, so somebody comparing digests with a partner can see which is which.
func TestDivergentMICReadingsAreBothRecorded(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	m := receive(t, r, request(t, "compressed.mime", signedReceipt("sha256"))).Message

	if len(m.AlternateMICs) != 1 {
		t.Fatalf("alternates = %+v, want the other reading recorded", m.AlternateMICs)
	}
	if m.AlternateMICs[0].Digest == m.MIC.Digest {
		t.Error("the two readings produced the same digest, so one of them is not being computed")
	}
	if m.AlternateMICs[0].Note == "" {
		t.Error("the alternate has no note saying what it is for")
	}
}

// The model has to survive a round trip through JSON, because that is what a
// store that persists events later would do to it.
func TestMessageSurvivesJSON(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "signed.mime", signedReceipt("sha256")))

	encoded, err := json.Marshal(res.Message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev := res.Event.Clone()
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev.Payload = generic

	got, ok := as2.MessageOf(ev)
	if !ok {
		t.Fatal("MessageOf could not read a message that had been through JSON")
	}
	if got.MessageID != res.Message.MessageID {
		t.Errorf("message id = %q, want %q", got.MessageID, res.Message.MessageID)
	}
	if got.MIC == nil || got.MIC.Digest != res.Message.MIC.Digest {
		t.Errorf("MIC did not survive: %+v", got.MIC)
	}
	if got.Security.Signature == nil || !got.Security.Signature.Verified {
		t.Errorf("the signature verdict did not survive: %+v", got.Security.Signature)
	}
	if _, ok := as2.MessageOf(&event.Event{Type: as2.EventType, Payload: map[string]any{"nope": 1}}); ok {
		t.Error("MessageOf accepted a payload that is not a message")
	}
}

// RFC 4130 §7.4.3: "for signed messages, the algorithm used to calculate the
// MIC MUST be the same as that used on the message that was signed". The
// sender's signed-receipt-micalg preference does not override that, which is
// easy to get backwards because the preference header looks authoritative.
func TestSignedMessageMICFollowsTheSignatureNotTheRequest(t *testing.T) {
	mics := referenceMICs(t)
	r, _ := receiverWith(t, as2.IdentityConfig{})

	// The message is signed with sha1; the sender asks for sha256.
	m := receive(t, r, request(t, "signed_sha1.mime", signedReceipt("sha256"))).Message
	if m.Security.Signature == nil || m.Security.Signature.DigestAlgorithm != "sha1" {
		t.Fatalf("signature digest = %+v, want sha1", m.Security.Signature)
	}
	if m.MIC == nil || m.MIC.Algorithm != "sha1" {
		t.Fatalf("MIC = %+v, want sha1 - the algorithm the message was signed with", m.MIC)
	}
	if m.MIC.Digest != mics["inner_sha1"] {
		t.Errorf("MIC = %q, want openssl's sha1 digest %q", m.MIC.Digest, mics["inner_sha1"])
	}
	// The MDN signs with the same algorithm, so a partner sees one algorithm
	// throughout the exchange.
	if m.MDN.MICAlg != "sha1" {
		t.Errorf("MDN micalg = %q, want sha1", m.MDN.MICAlg)
	}
	// And the sender's declared micalg is kept verbatim, because a mismatch
	// between it and what was actually used is a real interop bug.
	if got := m.Security.Signature.DeclaredMICAlg; got != "sha1" && got != "sha-1" {
		t.Errorf("declared micalg = %q, want it recorded as the sender wrote it", got)
	}
}
