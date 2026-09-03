package as2

import (
	"bytes"
	"compress/zlib"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
)

// AS2 1.1 adds a compression layer, RFC 5402, carried as the CMS
// compressed-data content type of RFC 3274. github.com/smallstep/pkcs7
// implements SignedData and EnvelopedData and nothing else, so this is
// hand-rolled - which turns out to be about sixty lines, because RFC 3274 is a
// genuinely small structure:
//
//	CompressedData ::= SEQUENCE {
//	  version CMSVersion,                              -- MUST be 0
//	  compressionAlgorithm CompressionAlgorithmIdentifier,
//	  encapContentInfo EncapsulatedContentInfo }
//
// wrapped, like every CMS object, in
//
//	ContentInfo ::= SEQUENCE {
//	  contentType ContentType,
//	  content [0] EXPLICIT ANY DEFINED BY contentType }
//
// The OIDs are read straight from RFC 3274's ASN.1 module; OpenSSL's own
// asn1parse resolves both by name ("id-smime-ct-compressedData", "zlib
// compression") in the fixture-generation script, which is an independent
// check that they are right.
//
// The compressed bytes are a raw zlib stream (RFC 1950), which is what
// compress/zlib reads.

var (
	// oidCompressedData is id-ct-compressedData, RFC 3274 §1.
	oidCompressedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 9}
	// oidZlibCompress is id-alg-zlibCompress, the only compression algorithm
	// RFC 3274 defines and the only one anybody uses.
	oidZlibCompress = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 3, 8}
	// oidPKCS7Data is id-data, the eContentType a compressed MIME entity
	// carries.
	oidPKCS7Data = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
)

// contentInfo is the CMS outer wrapper. Content is left as a RawValue so the
// explicit [0] tag can be unwrapped by hand: encoding/asn1 will not decode
// "ANY DEFINED BY" for us.
type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

type compressionAlgorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type encapsulatedContentInfo struct {
	EContentType asn1.ObjectIdentifier
	EContent     []byte `asn1:"explicit,optional,tag:0"`
}

type compressedData struct {
	Version              int
	CompressionAlgorithm compressionAlgorithmIdentifier
	EncapContentInfo     encapsulatedContentInfo
}

// ErrNotCompressedData says the DER is a well-formed CMS object of some other
// content type. The caller uses it to tell "this is not compressed data" apart
// from "this is compressed data and it is broken", which are different things
// to report to a partner.
var ErrNotCompressedData = errors.New("as2: not a CMS compressed-data object")

// CompressionInfo is what a compressed-data layer turned out to be. It is
// filled in even when decompression fails, because the partner still needs to
// be told which algorithm tommy could not handle, and the operator still needs
// to see that a layer was there.
type CompressionInfo struct {
	// Algorithm is the human name ("zlib") when recognized, otherwise "".
	Algorithm string `json:"algorithm,omitempty"`
	// AlgorithmOID is always set, so an unrecognized algorithm is still
	// reported precisely rather than as "unknown".
	AlgorithmOID string `json:"algorithm_oid"`
	// Placement is where the compression sat relative to the signature:
	// PlacementInner, PlacementOuter or PlacementOnly. RFC 5402 §3 allows
	// either side of the signature but "MUST NOT do both within the same
	// document", and a receiver must support both.
	Placement string `json:"placement"`
	// CompressedSize and DecompressedSize bracket what the layer bought.
	CompressedSize   int `json:"compressed_size"`
	DecompressedSize int `json:"decompressed_size,omitempty"`
	// Decompressed is false when tommy kept the compressed bytes instead of
	// expanding them, in which case Error says why.
	Decompressed bool   `json:"decompressed"`
	Error        string `json:"error,omitempty"`
}

// Compression placements, RFC 5402 §3.
const (
	// PlacementInner: the innermost document was compressed and the result
	// signed. The MIC then covers the compressed entity, because that is what
	// was signed.
	PlacementInner = "compressed-then-signed"
	// PlacementOuter: the complete signed structure was compressed. The MIC
	// still covers what was signed, which is the uncompressed inner entity.
	PlacementOuter = "signed-then-compressed"
	// PlacementOnly: compression with no signature anywhere.
	PlacementOnly = "compressed-only"
)

// Ratio is the space the compression saved, as a fraction, or 0 when unknown.
func (c CompressionInfo) Ratio() float64 {
	if c.DecompressedSize <= 0 {
		return 0
	}
	return 1 - float64(c.CompressedSize)/float64(c.DecompressedSize)
}

// Decompress reads a CMS compressed-data object and returns the MIME entity
// inside it.
//
// It returns the CompressionInfo whatever happens. An unknown compression
// algorithm, a corrupt zlib stream and a truncated encapContentInfo all come
// back as info with Decompressed false and Error set, plus a non-nil error -
// never as a silently dropped payload. Losing content quietly is the worst
// thing a capture tool can do, so the caller keeps the compressed bytes and
// records "tommy did not decompress this, and here is why".
func Decompress(der []byte) ([]byte, CompressionInfo, error) {
	info := CompressionInfo{CompressedSize: len(der)}

	var ci contentInfo
	if _, err := asn1.Unmarshal(der, &ci); err != nil {
		return nil, info, fmt.Errorf("as2: parse CMS ContentInfo: %w", err)
	}
	if !ci.ContentType.Equal(oidCompressedData) {
		return nil, info, fmt.Errorf("%w: content type is %s", ErrNotCompressedData, ci.ContentType)
	}

	var cd compressedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &cd); err != nil {
		return nil, info, fmt.Errorf("as2: parse CompressedData: %w", err)
	}
	info.AlgorithmOID = cd.CompressionAlgorithm.Algorithm.String()
	if cd.Version != 0 {
		// RFC 3274 fixes it at 0. Note it and carry on: a version nobody has
		// seen is not a reason to throw the content away. Decompressed stays
		// true if the bytes do inflate, so a note here reads as a remark
		// rather than a failure.
		info.Error = fmt.Sprintf("CompressedData version is %d, RFC 3274 requires 0", cd.Version)
	}
	if !cd.CompressionAlgorithm.Algorithm.Equal(oidZlibCompress) {
		info.Error = "unsupported compression algorithm " + info.AlgorithmOID
		return nil, info, fmt.Errorf("as2: %s", info.Error)
	}
	info.Algorithm = "zlib"

	if !cd.EncapContentInfo.EContentType.Equal(oidPKCS7Data) {
		// RFC 5402 wraps a MIME entity, which is id-data. Anything else is
		// worth naming rather than assuming, but it is not a reason to stop:
		// the bytes still inflate and the caller still wants them.
		info.Error = "compressed content type is " + cd.EncapContentInfo.EContentType.String() +
			", not id-data; the content was inflated anyway"
	}

	payload := cd.EncapContentInfo.EContent
	info.CompressedSize = len(payload)
	if len(payload) == 0 {
		info.Error = "compressed-data layer carries no content"
		return nil, info, errors.New("as2: " + info.Error)
	}

	zr, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		info.Error = "zlib stream is not readable: " + err.Error()
		return nil, info, fmt.Errorf("as2: open zlib stream: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var out bytes.Buffer
	if _, err := io.Copy(&out, io.LimitReader(zr, maxDecoded+1)); err != nil {
		info.Error = "zlib stream is corrupt: " + err.Error()
		return nil, info, fmt.Errorf("as2: inflate: %w", err)
	}
	if out.Len() > maxDecoded {
		info.Error = fmt.Sprintf("decompressed content exceeds %d bytes", maxDecoded)
		return nil, info, errors.New("as2: " + info.Error)
	}

	info.DecompressedSize = out.Len()
	info.Decompressed = true
	return out.Bytes(), info, nil
}

// isCompressedEntity reports whether a MIME entity is the RFC 5402 wrapper,
// "application/pkcs7-mime; smime-type=compressed-data". Some senders omit the
// smime-type parameter, so a pkcs7-mime entity named smime.p7z counts too.
func isCompressedEntity(e *Entity) bool {
	mt, params := e.MediaType()
	if mt != "application/pkcs7-mime" && mt != "application/x-pkcs7-mime" {
		return false
	}
	if params["smime-type"] == "compressed-data" {
		return true
	}
	return params["smime-type"] == "" && filenameSuffix(e) == ".p7z"
}
