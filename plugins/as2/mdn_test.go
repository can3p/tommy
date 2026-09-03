package as2_test

import (
	"strings"
	"testing"

	"github.com/smallstep/pkcs7"

	"github.com/can3p/tommy/plugins/as2"
)

// A signed MDN has to be verifiable by the partner that receives it, and the
// bytes the signature covers have to be the ones RFC 4130 §7.4.2 says: the
// whole multipart/report entity, starting at its own Content-Type header - the
// lines marked "&" in the RFC's Appendix A.2 example.
//
// This was confirmed independently with `openssl smime -verify` against
// tommy's certificate, which reported "Verification successful" and extracted
// content beginning at that Content-Type. This test is the regression guard for
// that structure: it re-splits the MDN with the plugin's own MIME layer, which
// is also the closest thing to what a partner's parser will do.
func TestSignedMDNVerifies(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "signed.mime", signedReceipt("sha256")))

	entity, err := as2.ParseEntity([]byte(
		"Content-Type: " + res.Response.Header.Get("Content-Type") + "\r\n\r\n" + string(res.Response.Body)))
	if err != nil {
		t.Fatalf("the MDN is not a parseable MIME entity: %v", err)
	}
	parts, err := entity.Parts()
	if err != nil {
		t.Fatalf("the MDN's multipart/signed will not split: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("the MDN has %d parts, want the report and the signature", len(parts))
	}

	report, sigPart := parts[0], parts[1]
	if mt, _ := report.MediaType(); mt != "multipart/report" {
		t.Errorf("part 1 is %q, want multipart/report", mt)
	}
	if _, params := report.MediaType(); params["report-type"] != "disposition-notification" {
		t.Errorf("report-type = %q", params["report-type"])
	}
	if mt, _ := sigPart.MediaType(); mt != "application/pkcs7-signature" {
		t.Errorf("part 2 is %q, want application/pkcs7-signature", mt)
	}

	der, err := sigPart.Decoded()
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	p7, err := pkcs7.Parse(der)
	if err != nil {
		t.Fatalf("parse signature: %v", err)
	}
	// The detached-signature pattern: assign the exact signed bytes, which are
	// the report entity as it appears between the delimiters.
	p7.Content = report.Raw
	if err := p7.Verify(); err != nil {
		t.Fatalf("the MDN's own signature does not verify over the report entity: %v", err)
	}

	signer := p7.GetOnlySigner()
	if signer == nil {
		t.Fatal("the MDN carries no signer certificate for the partner to check")
	}
	if got, want := as2.Fingerprint(signer), as2.Fingerprint(r.Identity().Certificate()); got != want {
		t.Errorf("the MDN was signed by %s, want tommy's own certificate %s", got, want)
	}

	// And the machine-readable part is inside the report, where a partner's
	// parser looks for it.
	reportParts, err := report.Parts()
	if err != nil {
		t.Fatalf("split the report: %v", err)
	}
	if len(reportParts) != 2 {
		t.Fatalf("the report has %d parts, want a human and a machine part", len(reportParts))
	}
	if mt, _ := reportParts[0].MediaType(); mt != "text/plain" {
		t.Errorf("the human-readable part is %q", mt)
	}
	if mt, _ := reportParts[1].MediaType(); mt != "message/disposition-notification" {
		t.Errorf("the machine-readable part is %q", mt)
	}
	if !strings.Contains(string(reportParts[1].Body), "Received-Content-MIC: "+res.Message.MIC.Header()) {
		t.Errorf("the disposition-notification does not carry the computed MIC:\n%s", reportParts[1].Body)
	}
}

// An unsigned MDN is the plain multipart/report, and the same structural rules
// apply to it.
func TestUnsignedMDNStructure(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	res := receive(t, r, request(t, "plain.mime", nil))

	if res.Message.Receipt.Requested {
		t.Fatal("no receipt was requested by this fixture")
	}
	// Nothing was asked for, so nothing is signed - but an MDN still goes back,
	// because a partner that gets a bare 200 learns nothing.
	if got := res.Response.Header.Get("Content-Type"); !strings.HasPrefix(got, "multipart/report;") {
		t.Fatalf("Content-Type = %q, want multipart/report", got)
	}
	entity, err := as2.ParseEntity([]byte(
		"Content-Type: " + res.Response.Header.Get("Content-Type") + "\r\n\r\n" + string(res.Response.Body)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts, err := entity.Parts()
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("the report has %d parts, want two", len(parts))
	}
}
