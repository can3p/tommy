package as2

import (
	"bytes"
	"crypto"

	// Registering the hash implementations with crypto.Hash. MD5 and SHA-1 are
	// here because RFC 4130 names them as the MIC algorithms and partners
	// still negotiate them; a MIC is an integrity check against accidental
	// corruption in transit, not a security boundary in tommy.
	_ "crypto/md5"  //nolint:gosec
	_ "crypto/sha1" //nolint:gosec
	_ "crypto/sha256"
	_ "crypto/sha512"

	"encoding/base64"
	"fmt"
	"strings"
)

// The Received-Content-MIC is the single most interop-sensitive number in AS2,
// and there are three different rules for what it covers. RFC 4130 §7.3.1:
//
//   - a signed message: the MIC covers the MIME header and content of the
//     signed part, canonicalized. In practice that is exactly the bytes the
//     sender signed, because the sender was required to canonicalize before
//     signing;
//   - encrypted but not signed: the MIC covers the decrypted MIME header and
//     content, canonicalized;
//   - neither signed nor encrypted: the MIC covers the content only, without
//     the MIME or RFC 2822 headers, "since these are sometimes altered or
//     reordered by Mail Transport Agents".
//
// RFC 5402 §4 then adds compression, and the rule that catches people out: for
// a signed message the MIC is "calculated over the same data that was signed",
// and the signed content may itself be a compressed-data entity. Decompressing
// before hashing there produces a MIC that every partner rejects while every
// self-consistent round-trip test passes. For the two unsigned cases 5402 says
// the opposite - the MIC covers the *uncompressed* content.
//
// RFC 5402 §4.3 and RFC 4130 §7.3.1 genuinely contradict each other for the
// plain unsigned case: 4130 says "without the MIME or any other RFC 2822
// headers", 5402 says "including all MIME header fields and any applied
// Content-Transfer-Encoding". 4130 is the normative AS2 standard and 5402 is
// Informational, so the MIC tommy returns follows 4130; the other reading is
// computed too and recorded as an alternate, because somebody chasing a MIC
// mismatch needs to see both numbers rather than be told one of them is right.

// MIC coverage codes: what the digest was actually taken over. They are stored
// on the message and shown in the UI, because "sha256 of what?" is the whole
// question when two implementations disagree.
const (
	// MICOverSignedContent: the exact bytes that were signed, headers
	// included, compressed if that is how they were signed.
	MICOverSignedContent = "signed-content"
	// MICOverDecryptedEntity: the decrypted MIME entity, headers included.
	MICOverDecryptedEntity = "decrypted-mime-entity"
	// MICOverContentOnly: the body alone, no MIME headers - RFC 4130's rule
	// for a message that was neither signed nor encrypted.
	MICOverContentOnly = "content-only"
	// MICOverFullEntity: body and MIME headers, RFC 5402 §4.3's reading of
	// the same unsigned case. Only ever recorded as an alternate.
	MICOverFullEntity = "full-mime-entity"
)

// MIC is a computed Received-Content-MIC: the digest, the algorithm it was
// taken with, and - the part the RFCs leave implicit and interop arguments
// turn on - what exactly it covered.
type MIC struct {
	Digest    string `json:"digest"`    // base64, as it goes on the wire
	Algorithm string `json:"algorithm"` // "sha256", "sha1", "md5", ...
	Coverage  string `json:"coverage"`  // one of the MICOver* codes
	Bytes     int    `json:"bytes"`     // how many bytes were hashed
	Note      string `json:"note,omitempty"`
}

// Header renders the MIC the way RFC 4130 §7.4.3 spells it:
// "<base64 digest>, <algorithm>".
func (m MIC) Header() string {
	if m.Digest == "" {
		return ""
	}
	return m.Digest + ", " + m.Algorithm
}

// Empty reports whether no digest was computed.
func (m MIC) Empty() bool { return m.Digest == "" }

// micAlgorithms maps the normalized algorithm name to its hash.
//
// RFC 4130 defines only sha1 and md5, and recommends sha1 outbound. That has
// not been true on the wire for years: every current partner negotiates
// sha256, and OpenSSL writes the micalg parameter as "sha-256" with a hyphen
// while the RFC's own grammar has no hyphen. Both spellings arrive, so both
// are accepted, and the MDN echoes back whichever algorithm was actually used
// rather than the one the RFC prefers.
var micAlgorithms = map[string]crypto.Hash{
	"md5":    crypto.MD5,
	"sha1":   crypto.SHA1,
	"sha224": crypto.SHA224,
	"sha256": crypto.SHA256,
	"sha384": crypto.SHA384,
	"sha512": crypto.SHA512,
}

// DefaultMICAlgorithm is what an unsigned message's MIC is taken with. RFC 4130
// §7.4.3: "If the message is not signed, then the SHA-1 algorithm SHOULD be
// used."
const DefaultMICAlgorithm = "sha1"

// NormalizeMICAlg turns any spelling a partner might send into the name used
// in the model and on the wire, and reports whether tommy can compute it.
//
// It accepts the RFC's "sha1"/"md5", OpenSSL's hyphenated "sha-256", the
// "sha2-256" form some Java stacks emit, and an OID for the handful of senders
// that put one in micalg.
func NormalizeMICAlg(s string) (string, bool) {
	v := strings.Trim(strings.ToLower(strings.TrimSpace(s)), `"`)
	if name, ok := micAlgorithmOIDs[v]; ok {
		return name, true
	}
	v = strings.NewReplacer("-", "", "_", "", " ", "").Replace(v)
	switch v {
	case "sha":
		v = "sha1"
	case "sha2224":
		v = "sha224"
	case "sha2256":
		v = "sha256"
	case "sha2384":
		v = "sha384"
	case "sha2512":
		v = "sha512"
	}
	_, ok := micAlgorithms[v]
	return v, ok
}

// micAlgorithmOIDs covers the senders that put an OID in micalg rather than a
// name. Rare, but a message tommy cannot name the algorithm of gets no MIC at
// all, and that is a silent-looking failure for the partner.
var micAlgorithmOIDs = map[string]string{
	"1.3.14.3.2.26":          "sha1",
	"1.2.840.113549.2.5":     "md5",
	"2.16.840.1.101.3.4.2.1": "sha256",
	"2.16.840.1.101.3.4.2.2": "sha384",
	"2.16.840.1.101.3.4.2.3": "sha512",
	"2.16.840.1.101.3.4.2.4": "sha224",
}

// ComputeMIC hashes data and returns the MIC, tagged with what it covers.
func ComputeMIC(alg, coverage string, data []byte) (MIC, error) {
	name, ok := NormalizeMICAlg(alg)
	if !ok {
		return MIC{}, fmt.Errorf("as2: unsupported MIC algorithm %q", alg)
	}
	h := micAlgorithms[name].New()
	h.Write(data)
	return MIC{
		Digest:    base64.StdEncoding.EncodeToString(h.Sum(nil)),
		Algorithm: name,
		Coverage:  coverage,
		Bytes:     len(data),
	}, nil
}

// Canonicalize applies the line-ending canonicalization RFC 4130 requires
// before a MIC is taken, and does no more than that.
//
// What it does: normalizes the *header block's* line endings to CRLF, and
// makes the header/body separator CRLFCRLF. RFC 4130 §7.3.1 asks for exactly
// this - "canonicalization on the MIME headers MUST be performed" - and it is
// safe because a header block is US-ASCII text by construction.
//
// What it deliberately does not do: touch the body. RFC 2045's canonical form
// normalizes line breaks for text/* types only, and an AS2 payload is normally
// application/edi-x12 or application/octet-stream, where a CR inserted into
// what the sender treated as binary would corrupt it. More importantly the
// sender signed these exact bytes: rewriting them can only move the digest
// away from the one the partner computed. Verified against openssl 3.6.1,
// whose signed content is the inner entity byte for byte with no
// normalisation of any kind applied to the body.
func Canonicalize(entity []byte) []byte {
	sep, sepLen := findSeparator(entity)
	if sep < 0 {
		return normalizeCRLF(entity)
	}
	out := make([]byte, 0, len(entity)+16)
	out = append(out, normalizeCRLF(entity[:sep])...)
	out = append(out, '\r', '\n', '\r', '\n')
	return append(out, entity[sep+sepLen:]...)
}

// CanonicalizeAll normalizes every line ending in the entity, body included.
// It is used only to compute the alternate MIC recorded when a sender did not
// canonicalize and the two readings disagree - never for the MIC tommy
// returns.
func CanonicalizeAll(entity []byte) []byte { return normalizeCRLF(entity) }

// normalizeCRLF turns lone LF and lone CR into CRLF and leaves existing CRLF
// alone.
func normalizeCRLF(b []byte) []byte {
	if !bytes.ContainsAny(b, "\r\n") {
		return b
	}
	out := make([]byte, 0, len(b)+len(b)/16)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\r':
			out = append(out, '\r', '\n')
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
		case '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, b[i])
		}
	}
	return out
}
