#!/usr/bin/env bash
#
# Regenerates every fixture in this directory using OpenSSL and nothing from
# tommy, which is the whole point: the parser in ../ is checked against an
# independent implementation of the crypto rather than against its own output.
#
# Run it from this directory. It is committed alongside its output so a future
# session can see exactly how these bytes were made, and remake them when a
# certificate finally expires (they are dated 20 years out, so not soon).
#
# Two things this script had to work around, both worth knowing:
#
#   1. Homebrew's OpenSSL 3.6.1 is built WITHOUT zlib, so `openssl cms
#      -compress` fails with "unsupported compression algorithm". The
#      RFC 3274 CompressedData fixtures are therefore assembled from OpenSSL's
#      own ASN.1 encoder (`asn1parse -genconf`) over a zlib stream produced by
#      Python. Both halves are still independent of the Go code under test, and
#      OpenSSL resolves the two OIDs by name in its parse output, which
#      double-checks them.
#   2. `openssl cms -sign -outform SMIME` writes its OUTER headers and its
#      multipart boundary delimiters with bare LF while the part headers and
#      body keep CRLF. That mixed-line-ending output is realistic - it is what
#      a partner using the OpenSSL command line actually sends - and it is why
#      the MIME splitter must accept a bare LF as the delimiter's line break.
#
set -euo pipefail

OS="${OPENSSL:-/opt/homebrew/bin/openssl}"
DAYS=7300

command -v python3 >/dev/null || { echo "python3 is required for the zlib streams" >&2; exit 1; }

# ---------------------------------------------------------------- identities
# partner.* signs; tommy.* is the receiver whose public key messages are
# encrypted to. stranger.* exists only to produce a message tommy cannot
# decrypt.
for who in partner tommy stranger; do
  case $who in
    partner)  subj="/CN=partner.example/O=Tommy AS2 Test Partner" ;;
    tommy)    subj="/CN=tommy.local/O=Tommy AS2 Test Receiver" ;;
    stranger) subj="/CN=stranger.example/O=Tommy AS2 Test Stranger" ;;
  esac
  "$OS" req -x509 -newkey rsa:2048 -keyout "$who.key" -out "$who.crt" \
        -days $DAYS -nodes -subj "$subj" -sha256 2>/dev/null
done

# ------------------------------------------------------------------- payload
printf 'ISA*00*          *00*          *ZZ*PARTNER        *ZZ*TOMMY          *260903*1200*U*00401*000000001*0*P*>~GS*PO*PARTNER*TOMMY*20260903*1200*1*X*004010~ST*850*0001~BEG*00*SA*PO-4711**20260903~PO1*1*10*EA*9.99**BP*WIDGET-1~CTT*1~SE*5*0001~GE*1*1~IEA*1*000000001~' > payload.edi

# inner.mime is the MIME entity an AS2 sender builds around the EDI document:
# CRLF headers, CRLF separator, the document byte for byte with no trailing
# newline. Every MIC in this directory is anchored to these 387 bytes.
{ printf 'Content-Type: application/edi-x12\r\n'
  printf 'Content-Transfer-Encoding: binary\r\n'
  printf 'Content-Disposition: attachment; filename=payload.edi\r\n'
  printf '\r\n'
  cat payload.edi
} > inner.mime

# The same entity written by a sender that never canonicalized: bare LF
# throughout. It exists to prove what canonicalization does and does not do.
python3 -c "
import sys
d = open('inner.mime','rb').read().replace(b'\r\n', b'\n')
open('inner_lf.mime','wb').write(d)
"

# --------------------------------------------------------------- unprotected
cp inner.mime plain.mime
cp inner_lf.mime plain_lf.mime

# -------------------------------------------------------------------- signed
"$OS" cms -sign -in inner.mime -signer partner.crt -inkey partner.key \
      -md sha256 -binary -outform SMIME -out signed.mime
"$OS" cms -sign -in inner.mime -signer partner.crt -inkey partner.key \
      -md sha1 -binary -outform SMIME -out signed_sha1.mime

# The enveloping ("opaque") signature form: the content travels inside the
# SignedData rather than beside it, as application/pkcs7-mime with
# smime-type=signed-data. Rare in AS2 but legal, and a message tommy could not
# read at all would be worse than one it reads unusually.
"$OS" cms -sign -in inner.mime -signer partner.crt -inkey partner.key \
      -md sha256 -binary -nodetach -outform SMIME -out signed_opaque.mime

# A signed message whose payload was altered in transit: the signature is
# structurally fine and cryptographically wrong, which is the case that must
# come back as processed/error: integrity-check-failed rather than a refusal.
python3 -c "
d = open('signed.mime','rb').read()
assert b'WIDGET-1' in d
open('signed_corrupt.mime','wb').write(d.replace(b'WIDGET-1', b'WIDGET-2', 1))
"

# ----------------------------------------------------------------- encrypted
"$OS" cms -encrypt -in inner.mime -aes-128-cbc -outform SMIME -out encrypted.mime tommy.crt
"$OS" cms -encrypt -in inner.mime -des3       -outform SMIME -out encrypted_3des.mime tommy.crt
"$OS" cms -encrypt -in signed.mime -aes-256-cbc -outform SMIME -out signed_encrypted.mime tommy.crt
# Encrypted to somebody else entirely: decryption must fail and be reported,
# not swallowed.
"$OS" cms -encrypt -in inner.mime -aes-128-cbc -outform SMIME -out undecryptable.mime stranger.crt

# ---------------------------------------------------------------- compressed
# compressed_der <infile> <outfile> [algorithm-oid]
compressed_der() {
  local src="$1" out="$2" alg="${3:-1.2.840.113549.1.9.16.3.8}"
  local hex cnf
  hex=$(python3 -c "
import zlib,sys
print(zlib.compress(open('$src','rb').read(), 9).hex())
")
  cnf=$(mktemp)
  cat > "$cnf" <<EOF
asn1 = SEQUENCE:ContentInfo

[ContentInfo]
contentType = OID:1.2.840.113549.1.9.16.1.9
content = EXPLICIT:0,SEQUENCE:CompressedData

[CompressedData]
version = INTEGER:0
compressionAlgorithm = SEQUENCE:CompressionAlgorithm
encapContentInfo = SEQUENCE:EncapsulatedContentInfo

[CompressionAlgorithm]
algorithm = OID:$alg

[EncapsulatedContentInfo]
eContentType = OID:1.2.840.113549.1.7.1
eContent = EXPLICIT:0,FORMAT:HEX,OCTETSTRING:$hex
EOF
  "$OS" asn1parse -genconf "$cnf" -out "$out" -noout
  rm -f "$cnf"
}

# compressed_entity <der> <outfile>: wrap a CompressedData DER in the RFC 5402
# MIME entity, base64 at 64 columns like every S/MIME tool emits.
compressed_entity() {
  { printf 'Content-Type: application/pkcs7-mime; smime-type=compressed-data; name=smime.p7z\r\n'
    printf 'Content-Transfer-Encoding: base64\r\n'
    printf 'Content-Disposition: attachment; filename=smime.p7z\r\n'
    printf '\r\n'
    # Folded in Python rather than with fold/sed: those leave the last line
    # without its terminator, so the entity ended with a bare CR. A body whose
    # final byte is a lone CR makes the RFC 2046 rule ambiguous - is that CR the
    # start of the delimiter's CRLF or the last byte of the content? - and no
    # real S/MIME tool emits one. Every line here ends CRLF, the last included.
    python3 -c "
import base64, sys
b = base64.b64encode(open(sys.argv[1], 'rb').read()).decode()
sys.stdout.write(''.join(b[i:i+64] + '\r\n' for i in range(0, len(b), 64)))
" "$1"
  } > "$2"
}

# Compressed only, no signature.
compressed_der inner.mime compressed.der
compressed_entity compressed.der compressed.mime

# Compression applied to the innermost document BEFORE signing (RFC 5402 §3,
# first placement). The MIC here covers the compressed entity, because that is
# what was signed - decompressing first is the classic interop bug.
"$OS" cms -sign -in compressed.mime -signer partner.crt -inkey partner.key \
      -md sha256 -binary -outform SMIME -out compressed_signed.mime

# Compression applied to the complete signed structure (second placement). The
# message is still signed, so its MIC covers inner.mime exactly as signed.mime's
# does - the two MICs must come out identical.
compressed_der signed.mime signed_compressed.der
compressed_entity signed_compressed.der signed_compressed.mime

# A CompressionAlgorithmIdentifier nobody implements. Must be recorded, and the
# payload kept, rather than dropped.
compressed_der inner.mime unknown_compression.der 1.2.840.113549.1.9.16.3.99
compressed_entity unknown_compression.der unknown_compression.mime
rm -f compressed.der signed_compressed.der unknown_compression.der

# ----------------------------------------------------------------- malformed
# A multipart/signed whose closing delimiter never arrives.
python3 -c "
d = open('signed.mime','rb').read()
i = d.rindex(b'------')
open('truncated.mime','wb').write(d[:i])
"
# A multipart declaring a boundary that appears nowhere in the body.
{ printf 'Content-Type: multipart/signed; protocol="application/pkcs7-signature"; micalg=sha-256; boundary="nowhere"\r\n'
  printf '\r\n'
  printf 'this body contains no delimiter at all\r\n'
} > no_boundary.mime

# ------------------------------------------------------- reference MIC values
# Computed by OpenSSL over the bytes OpenSSL itself considers signed, so the Go
# tests compare against a number this repository did not produce.
{
  printf '{\n'
  printf '  "_comment": "base64 digests computed by openssl, not by tommy. See generate.sh.",\n'
  printf '  "inner_sha256": "%s",\n'  "$("$OS" dgst -sha256 -binary inner.mime | base64)"
  printf '  "inner_sha1": "%s",\n'    "$("$OS" dgst -sha1   -binary inner.mime | base64)"
  printf '  "inner_md5": "%s",\n'     "$("$OS" dgst -md5    -binary inner.mime | base64)"
  printf '  "compressed_entity_sha256": "%s",\n' "$("$OS" dgst -sha256 -binary compressed.mime | base64)"
  printf '  "payload_only_sha256": "%s"\n' "$("$OS" dgst -sha256 -binary payload.edi | base64)"
  printf '}\n'
} > mic.json

# Prove OpenSSL agrees the signatures verify and that what it extracts from
# signed.mime is inner.mime byte for byte.
"$OS" cms -verify -in signed.mime -noverify -binary -out /tmp/as2check.bin 2>/dev/null
cmp -s /tmp/as2check.bin inner.mime || { echo "signed.mime does not carry inner.mime verbatim" >&2; exit 1; }
rm -f /tmp/as2check.bin inner_lf.mime

echo "fixtures regenerated"
