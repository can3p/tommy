package as2

import (
	"encoding/asn1"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/smallstep/pkcs7"
)

// The MDN is why AS2 belongs in tommy at all. Everything in it is derivable
// from the request - what arrived, whether it opened, what its digest is - so
// producing one is reporting, not simulating. Nothing here decides policy;
// every branch below is a fact about the message being restated in the form RFC
// 4130 §7.4 requires.
//
// The two shapes, from §7.4.2:
//
//	unsigned   Content-Type: multipart/report; report-type=disposition-notification
//	           part 1  text/plain                        - for a person
//	           part 2  message/disposition-notification  - for the software
//
//	signed     Content-Type: multipart/signed; micalg=…; protocol="application/pkcs7-signature"
//	           part 1  the entire multipart/report above, headers included
//	           part 2  application/pkcs7-signature, detached, over part 1
//
// And the header swap, from §6.2: the request's AS2-To becomes the response's
// AS2-From and the request's AS2-From becomes the response's AS2-To.

// Disposition modes and types, RFC 4130 §7.4.3. tommy is always automatic: no
// human approves an MDN here, so "manual-action" would be a lie.
const (
	DispositionMode      = "automatic-action/MDN-sent-automatically"
	DispositionProcessed = "processed"
	DispositionFailed    = "failed"
)

// DefaultReportingUA is the Reporting-UA field of a generated MDN.
const DefaultReportingUA = "tommy AS2"

// mdnOptions is what BuildMDN needs beyond the message itself.
type mdnOptions struct {
	identity    *Identity
	reportingUA string
	now         time.Time
	newID       func() string
	// host is used to make the MDN's own Message-ID look like a real one.
	host string
}

// dispositionFor renders the disposition-field value for a message.
//
// The rule that matters, and that is easy to get backwards: RFC 4130 §7.4.4
// says the "failed" disposition-type MUST NOT be used for a problem processing
// the message - only for a failure that prevents producing an MDN at all, such
// as being asked for a MIC algorithm that does not exist here (§7.5.3). A
// signature that will not verify or content that will not decrypt is
// "processed" with an error modifier (§7.5.4). So a message tommy could not
// open still comes back as processed, which is correct and initially surprising.
func dispositionFor(m *Message) string {
	if m.HasIssue(IssueUnsupportedMICAlgorithm) {
		return DispositionMode + "; " + DispositionFailed + "/Failure: unsupported MIC-algorithms"
	}
	if issue, ok := m.FirstError(); ok {
		return DispositionMode + "; " + DispositionProcessed + "/Error: " + errorModifier(issue.Code)
	}
	for _, i := range m.Issues {
		if i.Severity == SeverityWarning {
			return DispositionMode + "; " + DispositionProcessed + "/Warning: " + warningText(i)
		}
	}
	return DispositionMode + "; " + DispositionProcessed
}

// errorModifier maps an issue to one of the modifiers RFC 4130 §7.5.4 and
// RFC 5402 §5 define. Anything unmapped becomes unexpected-processing-error,
// which is what that modifier is for.
func errorModifier(code string) string {
	switch code {
	case IssueDecryptionFailed, IssueNoIdentity:
		return "decryption-failed"
	case IssueIntegrityCheckFailed:
		return "integrity-check-failed"
	case IssueDecompressionFailed, IssueUnsupportedCompression:
		return "decompression-failed"
	default:
		return "unexpected-processing-error"
	}
}

// warningText renders a warning modifier. RFC 4130 §7.5.5 predefines exactly
// one - "authentication-failed, processing continued" - and allows
// implementation-defined text otherwise.
func warningText(i Issue) string {
	if i.Code == IssueAuthenticationFailed {
		return "authentication-failed, processing continued"
	}
	return strings.Join(strings.Fields(i.Code+": "+i.Detail), " ")
}

// BuildMDN produces the synchronous MDN for a captured message: the HTTP
// response to write, and the record to keep on the message.
//
// It never fails in a way that stops a reply going out. A signature that cannot
// be produced downgrades the MDN to unsigned and says so in the record, because
// RFC 4130 §7.3.1 rule 2 allows an unsigned receipt when the requested
// signature cannot be produced, and a partner waiting on a connection that
// closes with nothing learns less than one that gets an unsigned receipt.
func BuildMDN(m *Message, opts mdnOptions) (Response, *MDNRecord) {
	boundary := "----=_tommy_" + opts.newID()
	sigBoundary := "----=_tommy_" + opts.newID() + "_signed"
	messageID := "<" + opts.newID() + "@" + mdnHost(opts.host) + ">"

	disposition := dispositionFor(m)
	human := humanText(m, disposition, opts.now)
	report := reportBody(m, disposition, boundary, human, opts)
	reportType := `multipart/report; report-type=disposition-notification; boundary="` + boundary + `"`

	rec := &MDNRecord{
		MessageID:   messageID,
		Disposition: disposition,
		HumanText:   human,
		Status:      http.StatusOK,
	}
	if m.MIC != nil {
		rec.ReceivedContentMIC = m.MIC.Header()
	}

	body := report
	contentType := reportType
	if m.Receipt.SignedRequested {
		signed, micalg, err := signMDN(reportType, report, m, opts, sigBoundary)
		if err == nil {
			body = signed
			contentType = fmt.Sprintf(`multipart/signed; micalg=%s; protocol="application/pkcs7-signature"; boundary="%s"`,
				micalg, sigBoundary)
			rec.Signed = true
			rec.MICAlg = micalg
		} else {
			// Say so in the human-readable part rather than silently
			// downgrading: the sender asked for a signature and did not get
			// one, and that is worth a sentence.
			rec.HumanText += "\r\nA signed receipt was requested but could not be produced: " + err.Error()
			human = rec.HumanText
			report = reportBody(m, disposition, boundary, human, opts)
			body = report
		}
	}

	resp := Response{
		Status: http.StatusOK,
		Header: http.Header{},
		Body:   []byte(body),
	}
	// RFC 4130 §6.2: the identifiers swap. Blank values are still emitted so a
	// partner sees which half of the exchange was missing, rather than a
	// header vanishing.
	resp.Header.Set("AS2-From", m.To)
	resp.Header.Set("AS2-To", m.From)
	resp.Header.Set("AS2-Version", Version)
	resp.Header.Set("Message-ID", messageID)
	resp.Header.Set("MIME-Version", "1.0")
	resp.Header.Set("Date", opts.now.UTC().Format(http.TimeFormat))
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Server", DefaultReportingUA)

	rec.Headers = resp.Header.Clone()
	rec.Size = int64(len(resp.Body))
	return resp, rec
}

func mdnHost(host string) string {
	if host == "" {
		return "tommy.local"
	}
	// A Message-ID's right-hand side must not carry a port or a path.
	if i := strings.IndexAny(host, ":/"); i > 0 {
		return host[:i]
	}
	return host
}

// reportBody assembles the multipart/report: a human part and a machine part.
func reportBody(m *Message, disposition, boundary, human string, opts mdnOptions) string {
	ua := opts.reportingUA
	if ua == "" {
		ua = DefaultReportingUA
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\r\n", args...) }

	w("--%s", boundary)
	w("Content-Type: text/plain; charset=us-ascii")
	w("Content-Transfer-Encoding: 7bit")
	w("")
	b.WriteString(normalizeToCRLFString(human))
	w("")

	w("--%s", boundary)
	w("Content-Type: message/disposition-notification")
	w("Content-Transfer-Encoding: 7bit")
	w("")
	w("Reporting-UA: %s", headerSafe(ua))
	// RFC 4130 §7.4.4: over HTTP the original and final recipients are the
	// same, and both are this receiver - the AS2-To of the request.
	if m.To != "" {
		w("Original-Recipient: rfc822; %s", headerSafe(m.To))
		w("Final-Recipient: rfc822; %s", headerSafe(m.To))
	}
	if m.MessageID != "" {
		// Byte for byte, angle brackets included: this is the field the sender
		// correlates its outbound message with, and normalising it is how an
		// MDN ends up matching nothing.
		w("Original-Message-ID: %s", headerSafe(m.MessageID))
	}
	w("Disposition: %s", disposition)
	// RFC 4130 §7.4.3: the MIC "is set only when the contents of the message
	// are processed successfully", so an error omits it rather than reporting
	// a digest of something that did not open.
	if _, failed := m.FirstError(); !failed && m.MIC != nil && !m.MIC.Empty() {
		w("Received-Content-MIC: %s", m.MIC.Header())
	}
	w("")

	w("--%s--", boundary)
	return b.String()
}

// humanText is the part a person reads first. RFC 4130's own examples put the
// message identifiers and a plain statement of the outcome here, and note that
// this part is the right place to explain an error in detail.
func humanText(m *Message, disposition string, now time.Time) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\r\n", args...) }

	w("This is an AS2 MDN from tommy, which captured the message rather than processing it as EDI.")
	w("")
	if m.MessageID != "" {
		w("  Message-ID: %s", headerSafe(m.MessageID))
	}
	if m.From != "" {
		w("  From:       %s", headerSafe(m.From))
	}
	if m.To != "" {
		w("  To:         %s", headerSafe(m.To))
	}
	if m.Subject != "" {
		w("  Subject:    %s", headerSafe(m.Subject))
	}
	w("  Received:   %s", now.UTC().Format(time.RFC1123Z))
	w("  Security:   %s", m.Security.Summary())
	w("")
	if sig := m.Security.Signature; sig != nil {
		w("%s", sig.Assurance())
		w("")
	}
	if len(m.Issues) > 0 {
		w("What tommy had to work around:")
		for _, i := range m.Issues {
			w("  [%s] %s: %s", i.Severity, i.Code, headerSafe(i.Detail))
		}
		w("")
	}
	w("Disposition: %s", disposition)
	w("")
	w("tommy stored the message and does not forward it anywhere. This receipt says what arrived,")
	w("not that any EDI translator accepted it.")
	return b.String()
}

// headerSafe strips CR and LF from a value that came off the wire before it is
// written into a header or into the human-readable part.
//
// This is the one place a captured value reaches a protocol boundary rather
// than a template, so it is the one place header injection could happen: an
// AS2-To containing a CRLF would otherwise let a sender write its own MDN
// fields. Control characters are replaced rather than dropped so the value
// still reads as wrong instead of quietly shortening.
func headerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, v)
}

func normalizeToCRLFString(s string) string {
	return string(normalizeCRLF([]byte(s)))
}

// signMDN wraps the multipart/report in a multipart/signed with a detached
// PKCS#7 signature, and returns the body plus the micalg parameter to declare.
func signMDN(reportType, report string, m *Message, opts mdnOptions, boundary string) (string, string, error) {
	cert, key, err := opts.identity.KeyPair()
	if err != nil {
		return "", "", err
	}

	// The signed content is the whole multipart/report entity: its own
	// Content-Type header, the blank line, then the parts. RFC 4130 §7.4.2 and
	// its Appendix A.2 example, where the lines marked "&" - the ones the
	// signature covers - start at the report's Content-Type.
	content := "Content-Type: " + reportType + "\r\n\r\n" + report

	alg, micalg := mdnSignatureAlgorithm(m)
	sd, err := pkcs7.NewSignedData([]byte(content))
	if err != nil {
		return "", "", err
	}
	// Per instance, never the package-level SetDefaultDigestAlgorithm: that is
	// a process-wide global and mutating it under a concurrent server would
	// let one request change another's digest.
	sd.SetDigestAlgorithm(alg)
	if err := sd.AddSigner(cert, key, pkcs7.SignerInfoConfig{}); err != nil {
		return "", "", err
	}
	// Detach before Finish, which is the order this library requires.
	sd.Detach()
	der, err := sd.Finish()
	if err != nil {
		return "", "", err
	}

	var b strings.Builder
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(content)
	b.WriteString("\r\n--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/pkcs7-signature; name=\"smime.p7s\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("Content-Disposition: attachment; filename=\"smime.p7s\"\r\n\r\n")
	b.WriteString(wrapBase64(der, 64))
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.String(), micalg, nil
}

// mdnSignatureAlgorithm picks the digest for the MDN's own signature and the
// micalg parameter that declares it.
//
// It follows the algorithm the message's MIC used, so a partner sees one
// algorithm throughout the exchange. MD5 is the exception: it is a MIC
// algorithm AS2 still allows but not one to sign a receipt with, so a message
// whose MIC is MD5 gets an SHA-256 signature.
//
// The micalg spelling is the S/MIME one (RFC 5751 §3.4.3.2): "sha-256" with the
// hyphen for the SHA-2 family, bare "sha1" and "md5" for the two RFC 4130 names.
// OpenSSL writes the hyphenated form too, which is the practical argument.
func mdnSignatureAlgorithm(m *Message) (asn1.ObjectIdentifier, string) {
	name := DefaultMICAlgorithm
	if m.MIC != nil && m.MIC.Algorithm != "" {
		name = m.MIC.Algorithm
	}
	switch name {
	case "sha1":
		return pkcs7.OIDDigestAlgorithmSHA1, "sha1"
	case "sha224":
		return pkcs7.OIDDigestAlgorithmSHA224, "sha-224"
	case "sha384":
		return pkcs7.OIDDigestAlgorithmSHA384, "sha-384"
	case "sha512":
		return pkcs7.OIDDigestAlgorithmSHA512, "sha-512"
	default:
		return pkcs7.OIDDigestAlgorithmSHA256, "sha-256"
	}
}

// wrapBase64 renders DER as base64 folded at n columns with CRLF, which is what
// every S/MIME implementation emits and what partner parsers expect to see.
func wrapBase64(der []byte, n int) string {
	encoded := base64.StdEncoding.EncodeToString(der)
	var b strings.Builder
	for i := 0; i < len(encoded); i += n {
		end := i + n
		if end > len(encoded) {
			end = len(encoded)
		}
		b.WriteString(encoded[i:end])
		if end < len(encoded) {
			b.WriteString("\r\n")
		}
	}
	return b.String()
}
