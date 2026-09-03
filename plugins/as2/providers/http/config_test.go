package http_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/as2"
)

// TestPartnerCertificate covers the distinction the whole plugin is built
// around: verifying a signature proves the bytes were not altered after the
// certificate inside the message signed them, and nothing about whose
// certificate that is. Only partner_cert_file turns the first claim into the
// second, so both halves are asserted here rather than assumed.
func TestPartnerCertificate(t *testing.T) {
	t.Run("the configured partner signed it", func(t *testing.T) {
		in := start(t, map[string]any{"partner_cert_file": key("partner.crt")})

		h, body := splitSMIME(t, cmsSign(t, ediEntity(sampleEDI), "sha256"))
		resp, mdn := post(t, in, h, body)
		assertMDNEnvelope(t, resp, mdn)

		m, _ := captured(t, in)
		sig := m.Security.Signature
		if sig == nil || !sig.Verified {
			t.Fatalf("signature = %+v, want verified", sig)
		}
		if !sig.PartnerConfigured || !sig.SignerMatched {
			t.Errorf("PartnerConfigured=%v SignerMatched=%v, want both true",
				sig.PartnerConfigured, sig.SignerMatched)
		}
		if len(m.Issues) != 0 {
			t.Errorf("issues = %+v, want none", m.Issues)
		}
	})

	t.Run("somebody else signed it", func(t *testing.T) {
		in := start(t, map[string]any{"partner_cert_file": key("partner.crt")})

		h, body := splitSMIME(t, cmsSignAs(t, ediEntity(sampleEDI), "sha256", "stranger"))
		resp, mdn := post(t, in, h, body)
		assertMDNEnvelope(t, resp, mdn)

		m, _ := captured(t, in)
		sig := m.Security.Signature
		if sig == nil || !sig.Verified {
			t.Fatalf("signature = %+v, want intact - the stranger signed it correctly", sig)
		}
		if !sig.PartnerConfigured || sig.SignerMatched {
			t.Errorf("PartnerConfigured=%v SignerMatched=%v, want true and false",
				sig.PartnerConfigured, sig.SignerMatched)
		}
		if !m.HasIssue(as2.IssueAuthenticationFailed) {
			t.Errorf("issues = %+v, want %s", m.Issues, as2.IssueAuthenticationFailed)
		}
		// RFC 4130 §7.5.5: a receiver that carries on anyway reports this as a
		// warning, not an error. Refusing would be a policy decision, and tommy
		// makes none.
		if !strings.Contains(m.MDN.Disposition, "processed/Warning: authentication-failed, processing continued") {
			t.Errorf("disposition = %q, want the §7.5.5 warning", m.MDN.Disposition)
		}
	})
}

// TestAS2ToPin covers the one setting that can express an opinion about who a
// message was for.
//
// What it must NOT do is refuse. RFC 4130 §6.2: "There is no required response
// to a client request containing invalid or unknown AS2-From or AS2-To header
// values. The receiving AS2 system MAY return an unsigned MDN with an
// explanation of the error, if the sending system requested an MDN." So a
// mismatch is recorded and shown, and the exchange completes exactly as it
// otherwise would - which is what these subtests pin down.
func TestAS2ToPin(t *testing.T) {
	send := func(t *testing.T, pin, as2To string) (*http.Response, []byte, map[string]any) {
		t.Helper()
		in := start(t, map[string]any{"as2_to": pin})
		h := http.Header{}
		h.Set("Content-Type", "application/edi-x12")
		h.Set("AS2-To", as2To)
		resp, mdn := post(t, in, h, []byte(sampleEDI))
		_, ev := captured(t, in)
		return resp, mdn, ev.Meta
	}

	t.Run("unset accepts anything", func(t *testing.T) {
		in := start(t, nil)
		h := http.Header{}
		h.Set("Content-Type", "application/edi-x12")
		h.Set("AS2-To", "SOMEBODY-ELSE")
		resp, mdn := post(t, in, h, []byte(sampleEDI))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		_, ev := captured(t, in)
		if _, ok := ev.Meta["as2_to_expected"]; ok {
			t.Error("an unpinned provider claimed to expect an identifier")
		}
		if !strings.Contains(string(mdn), "Original-Message-ID: "+messageID) {
			t.Error("no MDN came back")
		}
	})

	t.Run("matching", func(t *testing.T) {
		resp, mdn, meta := send(t, "TOMMY", "TOMMY")
		assertMDNEnvelope(t, resp, mdn)
		if meta["as2_to_matched"] != true {
			t.Errorf("as2_to_matched = %v, want true", meta["as2_to_matched"])
		}
		if meta["as2_to_expected"] != "TOMMY" {
			t.Errorf("as2_to_expected = %v", meta["as2_to_expected"])
		}
	})

	t.Run("mismatched is recorded, not refused", func(t *testing.T) {
		resp, mdn, meta := send(t, "TOMMY", "SOMEBODY-ELSE")
		// The exchange completes normally. This is the assertion worth having:
		// the temptation is to answer 401 or 404, and RFC 4130 §6.2 declines to
		// require any response at all.
		//
		// Note the receipt comes back FROM "SOMEBODY-ELSE": §6.2's header swap
		// is mechanical, so the MDN is addressed from whatever the sender put in
		// AS2-To. That is right, and it is also how a misconfigured sender sees
		// its own mistake reflected straight back at it.
		assertMDNEnvelopeFor(t, resp, mdn, "SOMEBODY-ELSE", "PARTNER")
		if meta["as2_to_matched"] != false {
			t.Errorf("as2_to_matched = %v, want false", meta["as2_to_matched"])
		}
		if meta["as2_to_expected"] != "TOMMY" {
			t.Errorf("as2_to_expected = %v", meta["as2_to_expected"])
		}
		if meta["as2_to"] != "SOMEBODY-ELSE" {
			t.Errorf("as2_to = %v, want what actually arrived", meta["as2_to"])
		}
	})

	t.Run("case is significant", func(t *testing.T) {
		// RFC 4130 §6.2 makes AS2 identifiers case-sensitive, so "tommy" and
		// "TOMMY" are two different partners. Folding them together here would
		// hide exactly the misconfiguration this setting exists to surface.
		_, _, meta := send(t, "TOMMY", "tommy")
		if meta["as2_to_matched"] != false {
			t.Errorf("as2_to_matched = %v for a case-differing identifier, want false", meta["as2_to_matched"])
		}
	})
}
