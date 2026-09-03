package as2

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/smallstep/pkcs7"
)

// The S/MIME half of AS2. Two rules shape everything here.
//
// First, tommy never refuses a message. A signature that does not verify, an
// unknown signer, content it cannot decrypt: all of those are captured, stored
// and shown, and reported honestly in the MDN. Refusing would leave the
// operator with nothing to look at, which is the opposite of the job.
//
// Second, and this is the distinction every read surface has to keep straight:
// verifying a signature proves the bytes were not altered after the certificate
// inside the message signed them. It proves nothing about who that certificate
// belongs to, because the certificate is self-attested - it traveled inside
// the message it authenticates. Only a partner certificate configured out of
// band turns "cryptographically intact" into "from the partner we expected".
// Signature.Verified is the first claim; Signature.SignerMatched is the second,
// and it is false whenever no partner certificate was configured.

// CertInfo describes a certificate found in a captured message, or tommy's own.
// It is the display form: everything a person needs to recognize a certificate,
// and nothing they would have to parse.
type CertInfo struct {
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	Serial       string    `json:"serial"`
	NotBefore    time.Time `json:"not_before"`
	NotAfter     time.Time `json:"not_after"`
	Fingerprint  string    `json:"fingerprint"` // SHA-256, colon-separated hex
	KeyAlgorithm string    `json:"key_algorithm,omitempty"`
	SelfSigned   bool      `json:"self_signed"`
}

// NewCertInfo summarizes a parsed certificate.
func NewCertInfo(c *x509.Certificate) CertInfo {
	if c == nil {
		return CertInfo{}
	}
	return CertInfo{
		Subject:      c.Subject.String(),
		Issuer:       c.Issuer.String(),
		Serial:       c.SerialNumber.String(),
		NotBefore:    c.NotBefore,
		NotAfter:     c.NotAfter,
		Fingerprint:  Fingerprint(c),
		KeyAlgorithm: c.PublicKeyAlgorithm.String(),
		SelfSigned:   c.Subject.String() == c.Issuer.String(),
	}
}

// Fingerprint is the SHA-256 of a certificate's DER as colon-separated
// uppercase hex - the form openssl x509 -fingerprint prints - so a person can
// compare tommy's certificate against the one their AS2 software imported
// without transcribing base64.
func Fingerprint(c *x509.Certificate) string {
	if c == nil {
		return ""
	}
	sum := sha256.Sum256(c.Raw)
	var b strings.Builder
	const hexDigits = "0123456789ABCDEF"
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteByte(hexDigits[v>>4])
		b.WriteByte(hexDigits[v&0x0f])
	}
	return b.String()
}

// Expired reports whether the certificate is outside its validity window at t.
// It is shown, never enforced: an expired partner certificate is exactly the
// kind of thing somebody runs tommy to discover.
func (c CertInfo) Expired(t time.Time) bool {
	if c.NotAfter.IsZero() {
		return false
	}
	return t.After(c.NotAfter) || t.Before(c.NotBefore)
}

// Signature is what a multipart/signed layer turned out to be.
type Signature struct {
	// Protocol is the declared protocol parameter, normally
	// "application/pkcs7-signature".
	Protocol string `json:"protocol,omitempty"`
	// DeclaredMICAlg is the micalg parameter exactly as the sender wrote it.
	// OpenSSL writes "sha-256"; RFC 4130's grammar has no hyphen. It is kept
	// verbatim because a mismatch between it and the digest algorithm actually
	// used is a real interop bug and worth seeing.
	DeclaredMICAlg string `json:"declared_micalg,omitempty"`
	// DigestAlgorithm is the algorithm the SignerInfo actually used,
	// normalized. This, not DeclaredMICAlg, is what the MIC is taken with.
	DigestAlgorithm string `json:"digest_algorithm,omitempty"`
	// SignatureAlgorithm is the OID of the algorithm the digest was signed
	// with.
	SignatureAlgorithm string `json:"signature_algorithm,omitempty"`
	// Signer is the certificate that signed, as carried inside the message.
	Signer *CertInfo `json:"signer,omitempty"`
	// Chain is every other certificate the message carried.
	Chain []CertInfo `json:"chain,omitempty"`
	// Verified: the signature is cryptographically valid over the bytes the
	// message carried. Nothing more.
	Verified bool `json:"verified"`
	// SignerMatched: the signing certificate is byte for byte the partner
	// certificate the operator configured.
	SignerMatched bool `json:"signer_matched"`
	// PartnerConfigured records whether there was anything to match against.
	// Without it SignerMatched=false is ambiguous between "wrong partner" and
	// "nobody said who the partner is", and those deserve different words.
	PartnerConfigured bool `json:"partner_configured"`
	// Error is why verification failed, when it did.
	Error string `json:"error,omitempty"`
}

// Assurance is the one sentence every read surface shows about what a signature
// actually proved. It lives here so no surface re-derives the distinction and
// none of them renders a tick that overstates it.
func (s *Signature) Assurance() string {
	switch {
	case s == nil:
		return "This message carried no signature, so nothing about its origin or its integrity was proven."
	case !s.Verified:
		return "The signature did not verify: the content does not match what the enclosed certificate signed."
	case s.SignerMatched:
		return "The signature is valid and the signing certificate is the configured partner certificate."
	case s.PartnerConfigured:
		return "The signature is valid over these bytes, but the signing certificate is not the configured partner certificate."
	default:
		return "The signature is valid over these bytes. Who signed is unproven: the certificate traveled inside the message, and no partner certificate is configured to check it against."
	}
}

// Encryption is what an enveloped-data layer turned out to be.
type Encryption struct {
	// ContentAlgorithm is the symmetric algorithm the payload was encrypted
	// with ("aes-128-cbc"), or "" when it could not be read.
	ContentAlgorithm    string `json:"content_algorithm,omitempty"`
	ContentAlgorithmOID string `json:"content_algorithm_oid,omitempty"`
	// KeyAlgorithm is how the content key was wrapped for the recipient.
	KeyAlgorithm    string `json:"key_algorithm,omitempty"`
	KeyAlgorithmOID string `json:"key_algorithm_oid,omitempty"`
	// Recipients names who the message was encrypted to, by issuer and serial.
	// Being encrypted to somebody else is the commonest reason decryption
	// fails, and this is what shows it at a glance.
	Recipients []RecipientInfo `json:"recipients,omitempty"`
	Decrypted  bool            `json:"decrypted"`
	Error      string          `json:"error,omitempty"`
}

// RecipientInfo identifies one recipient of an enveloped-data layer.
type RecipientInfo struct {
	Issuer string `json:"issuer"`
	Serial string `json:"serial"`
}

// symmetricAlgorithms are the content-encryption OIDs worth naming. The first
// six are what smallstep/pkcs7 can decrypt; the rest are here so a message
// tommy cannot decrypt still says what it was encrypted with, which is the
// difference between a useful failure and a shrug.
var symmetricAlgorithms = []struct {
	OID  asn1.ObjectIdentifier
	Name string
}{
	{asn1.ObjectIdentifier{1, 3, 14, 3, 2, 7}, "des-cbc"},
	{asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}, "des-ede3-cbc"},
	{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}, "aes-128-cbc"},
	{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}, "aes-256-cbc"},
	{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 6}, "aes-128-gcm"},
	{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46}, "aes-256-gcm"},
	{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}, "aes-192-cbc"},
	{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 26}, "aes-192-gcm"},
	{asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 2}, "rc2-cbc"},
}

// keyAlgorithms are the key-transport OIDs an AS2 partner uses to wrap the
// content key.
var keyAlgorithms = map[string]string{
	"1.2.840.113549.1.1.1":      "rsa",
	"1.2.840.113549.1.1.7":      "rsa-oaep",
	"1.2.840.113549.1.9.16.3.5": "esdh",
}

func symmetricName(oid asn1.ObjectIdentifier) string {
	for _, a := range symmetricAlgorithms {
		if a.OID.Equal(oid) {
			return a.Name
		}
	}
	return ""
}

// envelopedData is the slice of RFC 5652 EnvelopedData this code needs: who a
// message was encrypted to, and with what. smallstep/pkcs7 decrypts it but
// exposes neither, and both are things an operator staring at a failed
// decryption needs to see.
type envelopedData struct {
	Version              int
	OriginatorInfo       asn1.RawValue       `asn1:"optional,tag:0"`
	RecipientInfos       []recipientInfoASN1 `asn1:"set"`
	EncryptedContentInfo encryptedContentInfo
	UnprotectedAttrs     asn1.RawValue `asn1:"optional,tag:1"`
}

type recipientInfoASN1 struct {
	Version                int
	IssuerAndSerialNumber  issuerAndSerial
	KeyEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedKey           []byte
}

type issuerAndSerial struct {
	IssuerName   asn1.RawValue
	SerialNumber *big.Int
}

type encryptedContentInfo struct {
	ContentType                asn1.ObjectIdentifier
	ContentEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedContent           asn1.RawValue `asn1:"optional,tag:0"`
}

var oidEnvelopedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 3}

// inspectEnveloped reads what it can from an enveloped-data object without the
// private key.
//
// A structured parse is tried first. Senders that stream their CMS output emit
// indefinite-length BER rather than DER, which encoding/asn1 will not read at
// all; for those the content algorithm is recovered by looking for a known OID
// in the bytes, and Error says exactly that. Naming the algorithm approximately
// helps somebody debugging; letting it be mistaken for a full parse does not.
func inspectEnveloped(der []byte) Encryption {
	var enc Encryption

	var ci contentInfo
	if _, err := asn1.Unmarshal(der, &ci); err == nil && ci.ContentType.Equal(oidEnvelopedData) {
		var ed envelopedData
		if _, err := asn1.Unmarshal(ci.Content.Bytes, &ed); err == nil {
			oid := ed.EncryptedContentInfo.ContentEncryptionAlgorithm.Algorithm
			enc.ContentAlgorithmOID = oid.String()
			enc.ContentAlgorithm = symmetricName(oid)
			for _, r := range ed.RecipientInfos {
				if enc.KeyAlgorithmOID == "" {
					enc.KeyAlgorithmOID = r.KeyEncryptionAlgorithm.Algorithm.String()
					enc.KeyAlgorithm = keyAlgorithms[enc.KeyAlgorithmOID]
				}
				enc.Recipients = append(enc.Recipients, RecipientInfo{
					Issuer: rdnString(r.IssuerAndSerialNumber.IssuerName.FullBytes),
					Serial: r.IssuerAndSerialNumber.SerialNumber.String(),
				})
			}
			return enc
		}
	}

	for _, a := range symmetricAlgorithms {
		encoded, err := asn1.Marshal(a.OID)
		if err != nil || !bytes.Contains(der, encoded) {
			continue
		}
		enc.ContentAlgorithm = a.Name
		enc.ContentAlgorithmOID = a.OID.String()
		enc.Error = "this enveloped-data is streamed BER rather than DER, so its recipient list could not be read; " +
			"the content algorithm was identified by finding its OID in the bytes"
		break
	}
	return enc
}

// rdnString renders a DER-encoded Name for display, saying so when it will not
// parse: a distinguished name tommy cannot read is still worth reporting as
// present.
func rdnString(der []byte) string {
	var rdn pkix.RDNSequence
	if _, err := asn1.Unmarshal(der, &rdn); err != nil {
		return fmt.Sprintf("(unparseable name, %d bytes)", len(der))
	}
	var name pkix.Name
	name.FillFromRDNSequence(&rdn)
	return name.String()
}

// signerDigestAlgorithm reads the digest algorithm out of the first SignerInfo.
// That is the algorithm the MIC must be computed with: RFC 4130 §7.4.3, "for
// signed messages, the algorithm used to calculate the MIC MUST be the same as
// that used on the message that was signed". The micalg content-type parameter
// is what the sender claimed; this is what it did.
func signerDigestAlgorithm(p7 *pkcs7.PKCS7) string {
	if p7 == nil || len(p7.Signers) == 0 {
		return ""
	}
	name, ok := NormalizeMICAlg(p7.Signers[0].DigestAlgorithm.Algorithm.String())
	if !ok {
		return ""
	}
	return name
}

// signerSignatureAlgorithm names how the digest was signed, for display.
func signerSignatureAlgorithm(p7 *pkcs7.PKCS7) string {
	if p7 == nil || len(p7.Signers) == 0 {
		return ""
	}
	return p7.Signers[0].DigestEncryptionAlgorithm.Algorithm.String()
}

// certInfos summarizes every certificate a signature carried apart from the
// signer's own.
func certInfos(certs []*x509.Certificate, exclude *x509.Certificate) []CertInfo {
	var out []CertInfo
	for _, c := range certs {
		if exclude != nil && bytes.Equal(c.Raw, exclude.Raw) {
			continue
		}
		out = append(out, NewCertInfo(c))
	}
	return out
}
