# as2

## What it is

Tommy's AS2 content type. It stands in for a trading partner's AS2 endpoint:
it accepts the EDIINT messages your integration posts — signed, encrypted,
compressed, in any combination — unwraps them layer by layer, stores the EDI
document that was inside, and answers with the MDN receipt the sender is
waiting on.

AS2 is RFC 4130, with compression from RFC 5402 and RFC 3274. It belongs in
tommy because the reply is *mechanical*: everything in an MDN is derivable from
the request. Tommy reports what arrived; it does not decide policy about it.

## What it's for

Your integration posts an S/MIME-wrapped EDI document to a trading partner's
AS2 endpoint and blocks waiting on the synchronous MDN receipt before it
considers the send successful. Getting a *real* partner endpoint to test
against means a certificate exchange and a partner willing to be your test
target — neither of which exists yet when you're still writing the
integration. tommy stands in for that partner: fetching its certificate is
one `curl`, and every message it receives — whatever combination of signing,
encryption and compression it arrived in, however badly malformed — comes
back with a real MDN and is shown unwrapped layer by layer, with exactly what
the signature did and did not prove about who sent it.

## How to test it for real

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . as2 --ui-port 8811 --in-port 8822 --as2-in-memory
```

`--as2-in-memory` generates a throwaway key pair and writes nothing to disk —
use it (or `--as2-cert-dir <a scratch dir>`) for anything other than a real
long-running instance, since the default location is your real
`os.UserConfigDir()/tommy/as2`.

```bash
# Fetch tommy's certificate - every AS2 relationship starts here.
curl -sO http://localhost:8811/api/v1/as2/certificate

# Post a plain (unsigned, unencrypted) message and read the MDN back.
printf 'ISA*00*          *00*          *ZZ*PARTNER        *ZZ*TOMMY          *260903*1200*U*00401*000000001*0*P*>~SE*1*0001~IEA*1*000000001~' |
curl -s -D - --data-binary @- \
  -H 'AS2-From: PARTNER' -H 'AS2-To: TOMMY' -H 'AS2-Version: 1.1' \
  -H 'Message-ID: <1@partner.example>' \
  -H 'Content-Type: application/edi-x12' \
  -H 'Disposition-Notification-To: as2@partner.example' \
  http://localhost:8822/as2
```

For the full signed-and-encrypted exchange, verified against the returned
MDN with OpenSSL, see `providers/http/README.md`'s "Trying it" section — it
is executed, not just written down, and it is where the real trap (an
`-outform SMIME` body that reads as valid MIME but is not legal AS2 wire
format) is explained.

Read the captures back either through the plugin's own API or the generic
event feed:

```bash
curl -s http://localhost:8811/api/v1/as2/messages | jq '.[].meta.security'
curl -s 'http://localhost:8811/api/v1/events?plugin=as2' | jq length
```

Or open the tab: `http://localhost:8811/ui/as2/`.

## What is deliberately not implemented, in short

Only **synchronous** MDNs. `Receipt-Delivery-Option` asks for the receipt to
be POSTed back later to a URL of the sender's choosing, which means tommy
originating an outbound request — outside its charter of answering what it is
sent, never driving traffic. The header is recorded and an issue is raised
rather than silently ignored; see "What is deliberately not implemented"
below for the full reasoning.

## The three rules this package is built on

1. **Nothing is refused.** A signature that will not verify, content that will
   not decrypt, a compression algorithm nobody implements: each is captured,
   stored, shown, and reported honestly in the MDN's disposition. RFC 4130
   §7.4.4 agrees — `failed` is reserved for being unable to produce an MDN at
   all; a content problem is `processed` with an error modifier.
2. **Nothing is lost quietly.** When a layer cannot be opened, that layer's
   bytes become the payload with `Recovered` set, and an `Issue` records why.
3. **"Intact" and "from who it claims" are different claims.** Verifying a
   signature proves the bytes were not altered after the certificate *inside the
   message* signed them. It proves nothing about whose certificate that is.
   `Signature.Verified` is the first claim; `Signature.SignerMatched` is the
   second, and it is false unless a partner certificate was configured. Every
   read surface uses `Signature.Assurance()` rather than re-deriving it.

## The seam a provider codes against

Mirrors `files.Session`: the core does the work and appends the event, the
provider stays thin. A complete provider is this:

```go
type Provider struct {
    ident *as2.Identity
    recv  *as2.Receiver
}

// as2.IdentityBinder. The plugin calls this at construction time, which is how
// a provider is handed the shared key pair without the plugin importing it.
func (p *Provider) BindIdentity(id *as2.Identity) { p.ident = id }

func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
    // RegisterIngress is the ONLY place a provider has both a ProviderConfig
    // and a ConfigDir — RegisterAPI and RegisterUI are handed Deps with an
    // empty Config by contract — so this is where the identity is configured.
    // It is also what makes certificate generation lazy in the right way:
    // nothing is generated for a provider that is switched off.
    if err := p.ident.Configure(as2.IdentityConfig{
        CertFile:        d.Config.String("cert_file", ""),
        KeyFile:         d.Config.String("key_file", ""),
        PartnerCertFile: d.Config.String("partner_cert_file", ""),
        Dir:             d.Config.String("cert_dir", ""),
        ConfigDir:       d.ConfigDir,
    }); err != nil {
        d.Logger.Warn("as2: identity", "err", err)
        // Carry on. Messages are still captured; they just cannot be decrypted,
        // and the MDN says so.
    }

    p.recv = as2.NewReceiver(p.ident, d, as2.WithProvider(p.Name()))
    mux.HandleFunc("POST /as2", p.recv.Handle)
}
```

`Receiver.Handle` is the whole exchange: read the body, capture the message,
write the MDN. A provider that needs the message itself — to add its own
metadata, or to assert on it — calls `Receive` and writes the `Response`:

```go
res, err := p.recv.Receive(ctx, as2.Request{
    Method:   r.Method,
    Path:     r.URL.RequestURI(),
    Host:     r.Host,
    Header:   r.Header.Clone(),
    Body:     body,
    PeerAddr: r.RemoteAddr,
})
// res.Message is the canonical model, already stored.
// res.Event is the appended event.
// res.Response is the MDN: Status, Header, Body.
_ = res.Response.Write(w)
```

`Receive` returns an error **only** when the event could not be appended, which
is a tommy problem rather than a partner one. Everything a partner can get wrong
comes back as a 200 with an error disposition.

Receiver options: `WithProvider`, `WithReportingUA`, `WithMaxBody`, `WithMeta`.

`plugins/as2/fake_test.go` is a working provider built exactly this way. If it
stops compiling, this document is out of date.

### Identity

`Identity` is constructed unconfigured by the plugin and configured by the
provider. Where a generated certificate is kept, in order:

1. `IdentityConfig.Dir` — an explicitly configured directory;
2. `plugin.Deps.ConfigDir` — beside the config file this run was loaded from.
   Empty for every CLI shortcut and every test;
3. `os.UserConfigDir()/tommy/as2`.

The directory is created `0700`, the key file `0600`, and both are written to a
temporary file and renamed so a crash cannot leave a half-written key.
`IdentityConfig.InMemory` writes nothing at all — that is what tests use, and
what anyone who wants no files on disk should use, at the price of a partner
re-importing the certificate after every restart.

`CertFile`/`KeyFile` must be given together and the key must be **unencrypted**
PEM (PKCS#1 or PKCS#8). Tommy is a background process with no terminal to prompt
on, and a passphrase in a config file is worse than no passphrase; the error
message says `openssl pkey -in key.pem -out key-plain.pem`.

## Endpoints this plugin mounts

Under `/api/v1/as2/`:

| Route | What |
|---|---|
| `GET /messages` | Captured messages, newest first. Filters: `from`, `to`, `message_id`, `format`, `security`, `issue`, plus the core's `search`/`since`/`limit`/`offset`. |
| `GET /messages/{id}` | One message, unwrapped. |
| `GET /messages/{id}/raw` | The request exactly as it arrived — ciphertext included. |
| `GET /messages/{id}/payload` | The EDI document after every layer was peeled. |
| `GET /messages/{id}/mdn` | The MDN tommy returned, byte for byte. |
| `GET /certificate` | Tommy's certificate as PEM, for a partner to import. |
| `GET /identity` | Which certificate is in use, from where, and its fingerprint. |
| `DELETE /messages`, `DELETE /messages/{id}` | Clear. |

The tab is at `/ui/as2/`.

## Trying it

Once a provider is mounted, the exchange is two commands. Adjust the ingress
port and the provider's path to whatever your instance reports —
`tommy providers as2` prints both.

```bash
# 1. Give your partner (here, openssl) tommy's certificate.
curl -sO http://localhost:8811/api/v1/as2/certificate

# 2. Post a signed, encrypted EDI document and read the MDN.
printf 'Content-Type: application/edi-x12\r\n\r\nISA*00*...IEA*1*000000001~' > payload.mime

openssl cms -sign -in payload.mime -signer partner.crt -inkey partner.key \
        -md sha256 -binary -outform SMIME |
openssl cms -encrypt -aes-128-cbc -outform DER certificate |
  openssl base64 |                   # see the note below on why not -outform SMIME
  curl -s -D - --data-binary @- \
    -H 'AS2-From: PARTNER' \
    -H 'AS2-To: TOMMY' \
    -H 'AS2-Version: 1.1' \
    -H 'Message-ID: <1@partner.example>' \
    -H 'Content-Type: application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m' \
    -H 'Content-Transfer-Encoding: base64' \
    -H 'Disposition-Notification-To: as2@partner.example' \
    -H 'Disposition-Notification-Options: signed-receipt-protocol=optional,pkcs7-signature; signed-receipt-micalg=optional,sha256,sha1' \
    http://localhost:8822/as2
```

**Why `-outform DER | openssl base64` rather than `-outform SMIME`.** In AS2 the
`Content-Type` and `Content-Transfer-Encoding` are *HTTP* headers, so the request
body is bare base64 with no MIME header block of its own. `openssl cms -encrypt
-outform SMIME` writes three headers above its body, and the obvious way to get
rid of them — piping through `tail -n +2` — removes only `MIME-Version` and
leaves the other two sitting inside the body. Tommy then answers 200 with
`processed/Error: unexpected-processing-error` and `illegal base64 data at input
byte 7`, which looks enough like success to waste an afternoon. Asking for DER
and base64-ing it has no headers to strip and cannot go wrong this way.

`testdata/generate.sh` builds every fixture in this package the same way and is
the more complete worked example.

## What is deliberately not implemented

**Asynchronous MDNs.** `Receipt-Delivery-Option` asks for the receipt to be
delivered later to a URL of the sender's choosing. That means originating an
outbound HTTP request to a partner, which is outside tommy's charter — it
answers what is sent to it and never drives traffic. The header is recorded, an
`async-receipt-not-delivered` issue is raised so it is never silently ignored,
and a synchronous MDN is returned instead.

## The MIC, and why there are sometimes two

The `Received-Content-MIC` is the most interop-sensitive number in AS2 and there
are three rules for what it covers:

| Message | Digest covers | From |
|---|---|---|
| signed | exactly what was signed — **still compressed** if it was signed compressed | RFC 4130 §7.3.1, RFC 5402 §4.1 |
| encrypted, unsigned | the decrypted, decompressed entity, MIME headers included | RFC 4130 §7.3.1, RFC 5402 §4.2 |
| neither | RFC 4130 says the content alone, no MIME headers. RFC 5402 §4.3 says headers included. | — |

The last row is a genuine contradiction between the two documents. RFC 4130 is
the normative AS2 standard and RFC 5402 is Informational, so the MIC tommy
returns follows 4130 — except when compression is involved, which 4130 does not
address at all, where 5402's reading leads. The other reading is computed too
and shown as an *alternate*, because somebody chasing a MIC mismatch needs to
see both numbers rather than be told one of them is right.

`MIC.Coverage` always says which rule produced the digest.

The trap worth restating: for a **compress-then-sign** message the digest covers
the compressed entity. Expanding it first produces a MIC that every real partner
rejects while every self-consistent round-trip test passes.
`testdata/compressed_signed.mime` and `testdata/signed_compressed.mime` pin both
placements against digests OpenSSL computed.

## Fixtures

`testdata/` is generated by `testdata/generate.sh` using OpenSSL and nothing
from this repository, so the parser is checked against an independent
implementation of the crypto. Two things that script had to work around:

- **Homebrew's OpenSSL 3.6.1 is built without zlib**, so `openssl cms -compress`
  fails. The RFC 3274 `CompressedData` fixtures are assembled with OpenSSL's own
  ASN.1 encoder (`asn1parse -genconf`) over a zlib stream from Python. OpenSSL
  resolves both OIDs by name in its parse output, which double-checks them.
- **`openssl cms -sign -outform SMIME` mixes line endings**: bare LF for its
  outer headers and its multipart boundary delimiters, CRLF for the part headers
  and body. RFC 2046 specifies CRLF, so a strict splitter finds no parts at all
  in a message OpenSSL just produced. `mime.go` accepts either — and the line
  break immediately before a delimiter belongs to the delimiter, not to the
  content, which is what keeps every MIC the right length.
