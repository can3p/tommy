package as2

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/smallstep/pkcs7"
)

// Unwrapping an AS2 message is a loop, not a fixed shape. RFC 4130 and RFC 5402
// between them allow encryption around a signature, a signature around
// compression, compression around a signature, and any of those alone, so the
// only correct reading is: look at the outermost entity, peel whatever it is,
// look again. That is what unwrap does.
//
// Two things are decided here and nowhere else.
//
// The bytes the signature covers. Everything else about a message can be
// re-derived; these cannot, because they are a subslice of what arrived and the
// digest over them is what the partner is waiting to see echoed back. They are
// carried out of the loop as-is.
//
// Whether to keep going. Any layer that will not open ends the walk, and the
// unopened layer's bytes become the payload with Recovered set. The alternative -
// returning an error and storing nothing - would leave somebody debugging a
// failed AS2 exchange with a blank screen, which is the exact situation tommy
// exists to fix.

// analysis is what walking the layers produced.
type analysis struct {
	// payload is the innermost entity reached: the business document when
	// everything opened, otherwise the last layer that did.
	payload *Entity
	// signedBytes is exactly what the signature covered, or nil. For a
	// compress-then-sign message these are the compressed bytes, and hashing
	// anything else produces a MIC no partner agrees with (RFC 5402 §4.1).
	signedBytes []byte
	// signedMICNote explains a non-obvious choice - that the bytes had to be
	// canonicalized before the signature would verify, say - so the note ends
	// up on the MIC where somebody comparing digests will read it.
	signedMICNote string
}

// maxLayers stops a hostile or looping message from being peeled forever. Real
// AS2 nests at most three deep (encrypt(sign(compress(doc)))); eight leaves
// room for something unusual and none for a zip bomb of wrappers.
const maxLayers = 8

// unwrap peels an AS2 message down to its business document, recording what it
// found on m as it goes.
func unwrap(m *Message, top *Entity, id *Identity) analysis {
	a := walk(m, top, id)
	resolveCompressionPlacement(m)
	return a
}

// resolveCompressionPlacement can only be decided once the whole onion has been
// peeled, because RFC 5402 §3 allows compression on either side of the
// signature and which side it was on is a fact about their order. Layers are
// recorded outermost first, so a compressed layer listed before the signed one
// is compression wrapped around the signature.
func resolveCompressionPlacement(m *Message) {
	c := m.Security.Compression
	if c == nil {
		return
	}
	compressedAt, signedAt := -1, -1
	for i, l := range m.Layers {
		if l.Kind == LayerCompressed && compressedAt < 0 {
			compressedAt = i
		}
		if l.Kind == LayerSigned && signedAt < 0 {
			signedAt = i
		}
	}
	switch {
	case signedAt < 0:
		c.Placement = PlacementOnly
	case compressedAt < signedAt:
		c.Placement = PlacementOuter
	default:
		c.Placement = PlacementInner
	}
}

func walk(m *Message, top *Entity, id *Identity) analysis {
	var a analysis
	e := top

	for depth := 0; ; depth++ {
		if depth >= maxLayers {
			m.AddIssue(IssueMalformedMIME, SeverityError,
				fmt.Sprintf("message nests more than %d S/MIME layers deep", maxLayers))
			a.payload = e
			return a
		}

		switch {
		case isEnvelopedEntity(e):
			next, ok := peelEncrypted(m, e, id)
			if !ok {
				a.payload = e
				return a
			}
			e = next

		case isSignedMultipart(e):
			next, signed, note, ok := peelSignedMultipart(m, e, id)
			if !ok {
				a.payload = e
				return a
			}
			a.signedBytes, a.signedMICNote = signed, note
			e = next

		case isOpaqueSignedEntity(e):
			next, signed, ok := peelOpaqueSigned(m, e, id)
			if !ok {
				a.payload = e
				return a
			}
			a.signedBytes = signed
			e = next

		case isCompressedEntity(e):
			next, ok := peelCompressed(m, e)
			if !ok {
				a.payload = e
				return a
			}
			e = next

		default:
			m.Layers = append(m.Layers, Layer{
				Kind:        LayerPayload,
				ContentType: e.Get("Content-Type"),
				Bytes:       len(e.Raw),
				Opened:      true,
			})
			a.payload = e
			return a
		}
	}
}

// ---------------------------------------------------------------- encryption

func isEnvelopedEntity(e *Entity) bool {
	mt, params := e.MediaType()
	if mt != "application/pkcs7-mime" && mt != "application/x-pkcs7-mime" {
		return false
	}
	if params["smime-type"] == "enveloped-data" {
		return true
	}
	// Senders that omit smime-type are common enough that the filename
	// convention has to be honored; smime.p7m is enveloped or signed data,
	// and enveloped is what an AS2 outer layer is in practice.
	return params["smime-type"] == "" && filenameSuffix(e) == ".p7m"
}

func peelEncrypted(m *Message, e *Entity, id *Identity) (*Entity, bool) {
	layer := Layer{Kind: LayerEncrypted, ContentType: e.Get("Content-Type"), SMIMEType: e.SMIMEType(), Bytes: len(e.Raw)}
	m.Security.Encrypted = true

	der, err := e.Decoded()
	if err != nil {
		m.AddIssue(IssueTransferEncoding, SeverityError, err.Error())
	}

	enc := inspectEnveloped(der)
	m.Security.Encryption = &enc

	cert, key, idErr := id.KeyPair()
	if idErr != nil {
		enc.Error = "tommy has no certificate and key of its own, so nothing can be decrypted: " + idErr.Error()
		m.AddIssue(IssueNoIdentity, SeverityError, enc.Error)
		m.Layers = append(m.Layers, layer)
		return nil, false
	}

	p7, err := pkcs7.Parse(der)
	if err != nil {
		enc.Error = "enveloped-data will not parse: " + err.Error()
		m.AddIssue(IssueDecryptionFailed, SeverityError, enc.Error)
		m.Layers = append(m.Layers, layer)
		return nil, false
	}
	plain, err := p7.Decrypt(cert, key)
	if err != nil {
		enc.Error = decryptionDetail(err, enc)
		m.AddIssue(IssueDecryptionFailed, SeverityError, enc.Error)
		m.Layers = append(m.Layers, layer)
		return nil, false
	}

	enc.Decrypted = true
	layer.Opened = true
	m.Layers = append(m.Layers, layer)

	next, err := ParseEntity(plain)
	if err != nil && !errors.Is(err, ErrNoBody) {
		m.AddIssue(IssueMalformedMIME, SeverityError, "decrypted content is not a MIME entity: "+err.Error())
		return nil, false
	}
	return next, true
}

// decryptionDetail turns a library error into something an operator can act on.
// "no enveloped recipient for provided private key" is the single commonest AS2
// failure - the partner encrypted to a certificate that is not the one tommy
// holds - and saying so beats echoing the library.
func decryptionDetail(err error, enc Encryption) string {
	msg := err.Error()
	if !strings.Contains(msg, "recipient") {
		return "decryption failed: " + msg
	}
	detail := "this message was not encrypted to tommy's certificate"
	if len(enc.Recipients) > 0 {
		names := make([]string, 0, len(enc.Recipients))
		for _, r := range enc.Recipients {
			names = append(names, r.Issuer+" #"+r.Serial)
		}
		detail += ": it names " + strings.Join(names, ", ") +
			". Give the sender tommy's certificate, or point tommy at the key the sender already has."
	}
	return detail
}

// ----------------------------------------------------------- multipart/signed

func isSignedMultipart(e *Entity) bool {
	mt, _ := e.MediaType()
	return mt == "multipart/signed"
}

// peelSignedMultipart handles the detached signature form, which is what almost
// every AS2 partner sends: part one is the content, part two is an
// application/pkcs7-signature over it.
//
// The verification pattern matters. pkcs7.Parse of a detached signature yields
// a PKCS7 with no Content, so the exact signed bytes have to be assigned to
// p7.Content before Verify. Which exact bytes is the whole question: it is the
// first part as it arrived, delimiter line breaks excluded, headers included.
func peelSignedMultipart(m *Message, e *Entity, id *Identity) (*Entity, []byte, string, bool) {
	_, params := e.MediaType()
	layer := Layer{Kind: LayerSigned, ContentType: e.Get("Content-Type"), Bytes: len(e.Raw)}
	m.Security.Signed = true

	sig := &Signature{
		Protocol:       params["protocol"],
		DeclaredMICAlg: params["micalg"],
	}
	m.Security.Signature = sig

	parts, err := e.Parts()
	if err != nil && !errors.Is(err, errTruncatedMultipart) {
		sig.Error = err.Error()
		m.AddIssue(IssueMalformedMIME, SeverityError, "multipart/signed will not split: "+err.Error())
		m.Layers = append(m.Layers, layer)
		return nil, nil, "", false
	}
	if errors.Is(err, errTruncatedMultipart) {
		m.AddIssue(IssueTruncatedMultipart, SeverityWarning,
			"the multipart/signed body has no closing --boundary-- delimiter; the parts before it were used")
	}
	if len(parts) < 2 {
		sig.Error = fmt.Sprintf("multipart/signed carries %d part(s); a content part and a signature part are required", len(parts))
		m.AddIssue(IssueMalformedMIME, SeverityError, sig.Error)
		m.Layers = append(m.Layers, layer)
		if len(parts) == 1 {
			return parts[0], nil, "", true
		}
		return nil, nil, "", false
	}

	content := parts[0]
	sigPart := parts[len(parts)-1]
	der, err := sigPart.Decoded()
	if err != nil {
		sig.Error = err.Error()
		m.AddIssue(IssueTransferEncoding, SeverityError, "signature part: "+err.Error())
		m.Layers = append(m.Layers, layer)
		return content, content.Raw, "", true
	}

	signed, note := verifySignature(sig, der, content.Raw, id)
	recordSignatureIssues(m, sig)
	layer.Opened = true
	m.Layers = append(m.Layers, layer)
	return content, signed, note, true
}

// verifySignature checks a detached PKCS#7 signature over content and fills in
// sig. It returns the bytes the signature was found to cover, which is what the
// MIC must be taken over.
//
// It tries the content exactly as it arrived first, and only then the
// canonicalized form. That order is deliberate: the sender signed some specific
// sequence of bytes, and a sender that did not canonicalize before signing
// signed the un-canonicalized ones. Trying the canonical form first would fail
// on those and, worse, would silently produce a MIC over bytes nobody signed.
func verifySignature(sig *Signature, der, content []byte, id *Identity) ([]byte, string) {
	p7, err := pkcs7.Parse(der)
	if err != nil {
		sig.Error = "signature will not parse: " + err.Error()
		return content, ""
	}

	sig.DigestAlgorithm = signerDigestAlgorithm(p7)
	sig.SignatureAlgorithm = signerSignatureAlgorithm(p7)
	if signer := p7.GetOnlySigner(); signer != nil {
		info := NewCertInfo(signer)
		sig.Signer = &info
		sig.Chain = certInfos(p7.Certificates, signer)
		matchPartner(sig, signer.Raw, id)
	} else if len(p7.Certificates) > 0 {
		info := NewCertInfo(p7.Certificates[0])
		sig.Signer = &info
		sig.Chain = certInfos(p7.Certificates, p7.Certificates[0])
		matchPartner(sig, p7.Certificates[0].Raw, id)
	}

	p7.Content = content
	if err := p7.Verify(); err == nil {
		sig.Verified = true
		return content, ""
	} else {
		sig.Error = err.Error()
	}

	canonical := Canonicalize(content)
	if !bytes.Equal(canonical, content) {
		p7.Content = canonical
		if err := p7.Verify(); err == nil {
			sig.Verified = true
			sig.Error = ""
			return canonical, "the signed content had to be canonicalized to CRLF before the signature verified, " +
				"so the MIC covers the canonical form; the sender did not canonicalize before signing"
		}
	}
	return content, ""
}

// matchPartner decides whether the signer is who the operator expected.
//
// The comparison is on the certificate's DER, not a chain build. An AS2 partner
// certificate is exchanged out of band and is very often self-signed with no CA
// basic constraint, so x509 chain verification against it fails for reasons
// that have nothing to do with whether it is the right certificate. Byte
// equality is what AS2 software actually does and it is what "this is the
// certificate you gave me" means.
func matchPartner(sig *Signature, signerDER []byte, id *Identity) {
	partner := id.Partner()
	if partner == nil {
		return
	}
	sig.PartnerConfigured = true
	sig.SignerMatched = bytes.Equal(partner.Raw, signerDER)
}

func recordSignatureIssues(m *Message, sig *Signature) {
	switch {
	case !sig.Verified:
		detail := "the signature does not verify over the content that arrived"
		if sig.Error != "" {
			detail += ": " + sig.Error
		}
		m.AddIssue(IssueIntegrityCheckFailed, SeverityError, detail)
	case sig.PartnerConfigured && !sig.SignerMatched:
		m.AddIssue(IssueAuthenticationFailed, SeverityWarning,
			"the signature is valid but the signing certificate is not the configured partner certificate; "+
				"processing continued, as RFC 4130 §7.5.5 allows")
	}
}

// ------------------------------------------------------------- opaque signed

func isOpaqueSignedEntity(e *Entity) bool {
	mt, params := e.MediaType()
	if mt != "application/pkcs7-mime" && mt != "application/x-pkcs7-mime" {
		return false
	}
	return params["smime-type"] == "signed-data"
}

// peelOpaqueSigned handles the enveloping signature form, where the content is
// carried inside the SignedData rather than beside it. It is rare in AS2 but
// legal, and a message tommy could not read at all would be worse than one it
// reads slightly unusually.
func peelOpaqueSigned(m *Message, e *Entity, id *Identity) (*Entity, []byte, bool) {
	layer := Layer{Kind: LayerSigned, ContentType: e.Get("Content-Type"), SMIMEType: e.SMIMEType(), Bytes: len(e.Raw)}
	m.Security.Signed = true
	sig := &Signature{Protocol: "application/pkcs7-mime"}
	m.Security.Signature = sig

	der, err := e.Decoded()
	if err != nil {
		sig.Error = err.Error()
		m.AddIssue(IssueTransferEncoding, SeverityError, err.Error())
		m.Layers = append(m.Layers, layer)
		return nil, nil, false
	}
	p7, err := pkcs7.Parse(der)
	if err != nil {
		sig.Error = "signed-data will not parse: " + err.Error()
		m.AddIssue(IssueMalformedMIME, SeverityError, sig.Error)
		m.Layers = append(m.Layers, layer)
		return nil, nil, false
	}
	sig.DigestAlgorithm = signerDigestAlgorithm(p7)
	sig.SignatureAlgorithm = signerSignatureAlgorithm(p7)
	if signer := p7.GetOnlySigner(); signer != nil {
		info := NewCertInfo(signer)
		sig.Signer = &info
		sig.Chain = certInfos(p7.Certificates, signer)
		matchPartner(sig, signer.Raw, id)
	}
	if err := p7.Verify(); err == nil {
		sig.Verified = true
	} else {
		sig.Error = err.Error()
	}
	recordSignatureIssues(m, sig)

	layer.Opened = true
	m.Layers = append(m.Layers, layer)

	next, err := ParseEntity(p7.Content)
	if err != nil && !errors.Is(err, ErrNoBody) {
		m.AddIssue(IssueMalformedMIME, SeverityError, err.Error())
		return nil, p7.Content, false
	}
	return next, p7.Content, true
}

// --------------------------------------------------------------- compression

func peelCompressed(m *Message, e *Entity) (*Entity, bool) {
	layer := Layer{Kind: LayerCompressed, ContentType: e.Get("Content-Type"), SMIMEType: e.SMIMEType(), Bytes: len(e.Raw)}
	m.Security.Compressed = true

	der, err := e.Decoded()
	if err != nil {
		m.AddIssue(IssueTransferEncoding, SeverityError, err.Error())
		m.Layers = append(m.Layers, layer)
		return nil, false
	}

	plain, info, err := Decompress(der)
	// Placement is filled in by resolveCompressionPlacement once the whole
	// onion has been peeled: which side of the signature this sat on is not
	// knowable until the signature has been found, or found not to exist.
	m.Security.Compression = &info

	if err != nil {
		code := IssueDecompressionFailed
		if info.Algorithm == "" && info.AlgorithmOID != "" {
			code = IssueUnsupportedCompression
		}
		m.AddIssue(code, SeverityError,
			"the compressed-data layer was kept as it arrived because tommy could not expand it: "+info.Error)
		m.Layers = append(m.Layers, layer)
		return nil, false
	}

	layer.Opened = true
	m.Layers = append(m.Layers, layer)

	next, err := ParseEntity(plain)
	if err != nil && !errors.Is(err, ErrNoBody) {
		m.AddIssue(IssueMalformedMIME, SeverityError, "decompressed content is not a MIME entity: "+err.Error())
		return nil, false
	}
	return next, true
}

// ------------------------------------------------------------- MIC selection

// planMIC decides which bytes the Received-Content-MIC covers, and with which
// algorithm, and computes the alternate reading where the specifications
// disagree.
//
// The three rules, and where each comes from:
//
//   - signed: over exactly what was signed, with the algorithm the signature
//     used (RFC 4130 §7.3.1 and §7.4.3, RFC 5402 §4.1). If the signed content
//     was itself compressed it stays compressed here - expanding it first is
//     the classic AS2 interop bug, because the sender hashed the compressed
//     form;
//   - encrypted but unsigned: over the decrypted, decompressed entity, headers
//     included (RFC 4130 §7.3.1, refined by RFC 5402 §4.2);
//   - neither: RFC 4130 §7.3.1 says the content alone, without MIME headers,
//     while RFC 5402 §4.3 says headers included. 4130 is the normative AS2
//     standard, so it decides; the other reading is recorded as an alternate.
//     When compression was involved 5402 is the only specification that
//     addresses the case at all, so its reading leads and 4130's is the
//     alternate.
func planMIC(m *Message, a analysis, requested []string) {
	alg := micAlgorithmFor(m, requested)
	if alg == "" {
		m.AddIssue(IssueUnsupportedMICAlgorithm, SeverityError,
			"none of the MIC algorithms this message asked for is one tommy can compute: "+strings.Join(requested, ", "))
		return
	}

	var primary, alternate []byte
	var primaryCoverage, alternateCoverage string
	switch {
	case a.signedBytes != nil:
		primary, primaryCoverage = a.signedBytes, MICOverSignedContent
	case m.Security.Encrypted:
		primary, primaryCoverage = a.payload.Raw, MICOverDecryptedEntity
	case m.Security.Compressed:
		primary, primaryCoverage = a.payload.Raw, MICOverFullEntity
		alternate, alternateCoverage = a.payload.Body, MICOverContentOnly
	default:
		primary, primaryCoverage = a.payload.Body, MICOverContentOnly
		alternate, alternateCoverage = a.payload.Raw, MICOverFullEntity
	}
	if a.payload == nil {
		return
	}

	mic, err := ComputeMIC(alg, primaryCoverage, primary)
	if err != nil {
		m.AddIssue(IssueUnsupportedMICAlgorithm, SeverityError, err.Error())
		return
	}
	mic.Note = a.signedMICNote
	m.MIC = &mic

	if alternate != nil && !bytes.Equal(alternate, primary) {
		if alt, err := ComputeMIC(alg, alternateCoverage, alternate); err == nil {
			alt.Note = "the same content under the other reading of the specification; " +
				"a partner that computes this number rather than the one above is following " +
				"RFC 5402 §4.3 where tommy followed RFC 4130 §7.3.1, or the reverse"
			m.AlternateMICs = append(m.AlternateMICs, alt)
		}
	}
}

// micAlgorithmFor picks the digest.
//
// A signature settles it: RFC 4130 §7.4.3 requires the MIC algorithm to be the
// one the message was signed with, and the sender's signed-receipt-micalg
// preference does not override that. Otherwise the first algorithm the sender
// listed that tommy can compute wins, and failing that RFC 4130's recommended
// sha1.
func micAlgorithmFor(m *Message, requested []string) string {
	if sig := m.Security.Signature; sig != nil && sig.DigestAlgorithm != "" {
		return sig.DigestAlgorithm
	}
	for _, r := range requested {
		if name, ok := NormalizeMICAlg(r); ok {
			return name
		}
	}
	if len(requested) > 0 {
		// Everything the sender named is unsupported. Saying so is more useful
		// than quietly answering in an algorithm nobody asked for.
		return ""
	}
	return DefaultMICAlgorithm
}
