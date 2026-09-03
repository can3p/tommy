# as2/http

## What it is

The AS2-over-HTTP binding of RFC 4130: the route a trading partner POSTs an
EDIINT message to, and the certificate endpoint that tells it what to encrypt to.

It is named for the transport rather than for a vendor, because AS2 has no
vendor — it is a standard, and every partner's software speaks the same wire
format. The sibling bindings, AS3 over FTP and AS4 over SOAP, are different
transports of the same idea and would be different providers here.

The provider is thin on purpose. Everything hard — peeling the S/MIME layers,
computing the `Received-Content-MIC` over the right bytes, building and signing
the MDN, storing the document — lives in the plugin core behind `as2.Receiver`.
See `../../README.md` for how that works and what it deliberately does not do.

## What it's for

Standing up a real AS2 trading-partner relationship to test against means a
certificate exchange and a partner on the other end willing to be your test
target — usually neither exists yet while an integration is still being
written. This provider is that partner: `GET /as2/certificate` is the one
command that starts the relationship, and `POST /as2` accepts whatever your
software sends — signed, encrypted, compressed, any combination, or none —
and always answers with a real MDN, so a client waiting synchronously on that
receipt gets one. When something arrives malformed or cannot be decrypted,
the response says so honestly rather than hanging up or refusing, which is
usually the more useful case to test: knowing what your own integration does
when a partner's MDN reports a problem.

## How to test it for real

Both snippets below are executed against a live instance by
`TestSnippetsActuallyRun`, not merely rendered. Substitute the ingress port
your instance reports — `tommy providers as2` prints it, or start one at a
known port with `TOMMY_NO_UPDATE_CHECK=1 go run . as2 --ui-port 8811
--in-port 8822 --as2-in-memory` (`--as2-in-memory` avoids writing a generated
certificate to your real config directory, which otherwise defaults to
`os.UserConfigDir()/tommy/as2`).

### A plain message, from a cold start

Unsigned and unencrypted is legal AS2, so the first thing anyone runs needs no
keys and no setup.

```bash
printf 'ISA*00*          *00*          *ZZ*PARTNER        *ZZ*TOMMY          *260903*1200*U*00401*000000001*0*P*>~SE*1*0001~IEA*1*000000001~' |
curl -s -D - --data-binary @- \
  -H 'AS2-From: PARTNER' \
  -H 'AS2-To: TOMMY' \
  -H 'AS2-Version: 1.1' \
  -H 'Message-ID: <1@partner.example>' \
  -H 'Content-Type: application/edi-x12' \
  -H 'Disposition-Notification-To: as2@partner.example' \
  http://localhost:8822/as2
```

### Signed and encrypted, with the MDN verified

`openssl` here means a real OpenSSL, not LibreSSL — macOS ships LibreSSL as
`/usr/bin/openssl`, which cannot do everything below. Homebrew's
`/opt/homebrew/bin/openssl` (verified against 3.6.1) works; `$OPENSSL` in the
package's own tests overrides both. Homebrew's build has no zlib, so if you
were tempted to add `openssl cms -compress` to this chain, it fails outright
with "unsupported compression algorithm" — compressed fixtures in this
package are built a different way; see `../../README.md`'s Fixtures section.

```bash
# 1. Fetch tommy's certificate. Every AS2 relationship starts here.
curl -s -o tommy.pem http://localhost:8822/as2/certificate

# 2. A throwaway key pair standing in for your AS2 software's own.
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 365 \
  -subj /CN=partner.example -keyout partner.key -out partner.crt 2>/dev/null

# 3. The MIME entity around the EDI document, signed, then encrypted to tommy.
#    The signed entity keeps its own MIME headers - those are part of what the
#    signature covers - but the ENCRYPTED result must reach the wire as bare
#    base64: in AS2 the Content-Type and Content-Transfer-Encoding are HTTP
#    headers, so openssl's own S/MIME headers would end up inside the body.
#    Asking for DER and base64-ing it is unambiguous; stripping headers off
#    -outform SMIME output is where this goes wrong.
printf 'Content-Type: application/edi-x12\r\n\r\nISA*00*...IEA*1*000000001~' > payload.mime
openssl cms -sign -in payload.mime -signer partner.crt -inkey partner.key \
        -md sha256 -binary -outform SMIME |
  openssl cms -encrypt -aes-128-cbc -outform DER tommy.pem |
  openssl base64 > message.p7m

# 4. Post it. -D keeps the MDN's headers, which carry the Content-Type the
#    receipt's multipart boundary lives in.
curl -s -o mdn.body -D mdn.head --data-binary @message.p7m \
  -H 'AS2-From: PARTNER' -H 'AS2-To: TOMMY' -H 'AS2-Version: 1.1' \
  -H 'Message-ID: <2@partner.example>' \
  -H 'Content-Type: application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m' \
  -H 'Content-Transfer-Encoding: base64' \
  -H 'Disposition-Notification-To: as2@partner.example' \
  -H 'Disposition-Notification-Options: signed-receipt-protocol=optional,pkcs7-signature; signed-receipt-micalg=optional,sha256' \
  http://localhost:8822/as2

# 5. Reattach that Content-Type to the body and let OpenSSL check the signature.
#    The MDN is multipart/signed, so its boundary is in the HTTP header rather
#    than in the body; putting the two back together makes it a MIME message
#    again. -purpose any is not needed for tommy's generated certificate, but
#    keeps this working with a cert_file whose extended key usage does not name
#    S/MIME - which plenty of real AS2 certificates do not.
{ printf '%s\r\n\r\n' "$(grep -i '^content-type:' mdn.head | tr -d '\r')"
  cat mdn.body; } > mdn.mime
openssl smime -verify -in mdn.mime -CAfile tommy.pem -purpose any -out mdn.report

# mdn.report carries the disposition and the Received-Content-MIC your software
# compares against its own digest of what it signed.
grep -E 'Disposition|Received-Content-MIC' mdn.report
```

### The trap in step 3

`openssl cms -encrypt -outform SMIME` writes four MIME headers of its own —
`MIME-Version`, `Content-Disposition`, `Content-Type` and
`Content-Transfer-Encoding` — followed by a blank line and the base64. In AS2
those headers belong in the **HTTP request**, not in the body: there is no second
header block inside an AS2 message. Dropping only the first line (`tail -n +2`,
which is the obvious thing to reach for) leaves the other three in the body,
where they are not base64, and the receiver answers
`processed/Error: unexpected-processing-error` with
`illegal base64 data at input byte 7`. Asking for DER and encoding it yourself
sidesteps the question entirely.

Read the capture back either through the plugin's own API or the generic
event feed, and via the UI tab:

```bash
curl -s http://localhost:8811/api/v1/as2/messages | jq '.[].meta.security'
curl -s 'http://localhost:8811/api/v1/events?plugin=as2' | jq length
```

## Endpoints

| Route | What |
|---|---|
| `POST /as2` | Accept an AS2 message and answer with a synchronous MDN receipt. Signed, encrypted and compressed messages are unwrapped; anything that cannot be opened is still captured and reported honestly in the MDN's disposition rather than refused. |
| `GET /as2/certificate` | Serve tommy's certificate as PEM, before any message has arrived. This is where an exchange starts. |

**The paths are not configurable, and that is a core limitation rather than a
choice.** `plugin.Provider.Endpoints()` takes no `Deps`, so a provider cannot
declare a route whose path depends on configuration, and the ingress refuses to
start when a mounted route is not declared. Real AS2 partners do configure each
other's URLs. It has been reported rather than worked around.

## Configuration

```toml
[plugins.as2.providers.http]
enabled           = true
cert_file         = "/etc/tommy/as2-cert.pem"   # PEM certificate; needs key_file
key_file          = "/etc/tommy/as2-key.pem"    # unencrypted PEM key (PKCS#1 or PKCS#8)
partner_cert_file = "/etc/tommy/partner.pem"    # what inbound signatures are checked against
cert_dir          = "/var/lib/tommy/as2"        # where a generated pair is kept
common_name       = "tommy AS2"                 # subject of a generated certificate
in_memory         = false                       # generate a pair and write nothing at all
as2_to            = "TOMMY"                     # the identifier this endpoint answers to
max_body          = 67108864                    # cap on a captured message, in bytes
```

Every setting is optional. With none of them, a key pair is generated on first
start and kept beside the config file, or in `os.UserConfigDir()/tommy/as2` when
the config was built in memory.

`cert_file` and `key_file` must be given together and the key must be
**unencrypted** — tommy is a background process with no terminal to prompt on,
and a passphrase in a config file is worse than no passphrase. Convert one with
`openssl pkey -in key.pem -out key-plain.pem`.

`partner_cert_file` is what turns "this signature is intact" into "this is who it
says it is". Without it a signature can be shown to be valid and never
attributed, and every read surface says so.

`as2_to` **does not refuse anything.** RFC 4130 §6.2: *"There is no required
response to a client request containing invalid or unknown AS2-From or AS2-To
header values."* So a message addressed elsewhere still gets a receipt and is
still captured; the mismatch is recorded on the event as `as2_to_expected` and
`as2_to_matched` so it is visible in the tab instead of being a green light. The
comparison is byte-exact, because §6.2 makes AS2 identifiers case-sensitive.

## Testing

Everything in this package is driven by OpenSSL and curl over a real socket.
There is no Go AS2 client in existence, so a test that built its messages with
tommy's own code would only prove tommy agrees with itself — the same blind spot
that let ftpserverlib silently corrupt every download in this project until
somebody ran curl. Messages are built with `openssl cms`, the returned MDN is
handed to `openssl smime -verify`, and every `Received-Content-MIC` is compared
against a digest `openssl dgst` computed.

The tests skip cleanly when `openssl` or `curl` is absent, because a green run on
a machine that could not check anything is worse than a red one. macOS ships
LibreSSL as `/usr/bin/openssl`; the suite prefers a Homebrew OpenSSL and
`$OPENSSL` overrides both.
