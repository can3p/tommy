// Package http is the AS2-over-HTTP binding of RFC 4130: the route a trading
// partner POSTs an EDIINT message to, and the certificate endpoint that lets it
// find out what to encrypt to.
//
// It is named for the transport rather than for a vendor because AS2 has no
// vendor - it is a standard, and every partner's software speaks the same wire
// format. RFC 4130 is "MIME-based Secure Peer-to-Peer Business Data Interchange
// Using HTTP"; the sibling bindings, AS3 over FTP and AS4 over SOAP, are
// different transports of the same idea and would be different providers here.
//
// The provider is deliberately thin. Everything that is actually hard - peeling
// the S/MIME layers, computing the Received-Content-MIC over the right bytes,
// building and signing the MDN, storing the document - lives in the as2 plugin
// core, behind as2.Receiver. What is left is the route, the configuration, the
// discovery surface, and one policy question the core cannot answer for itself
// (see as2To below).
package http

import (
	"fmt"
	"net/http"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/as2"
)

// Name is the provider's segment in the registry and on the command line.
const Name = "http"

// Route paths.
//
// These are constants rather than settings, and that is a real limitation worth
// naming: plugin.Provider.Endpoints() takes no Deps, so a provider cannot
// declare a route whose path depends on configuration - and the ingress refuses
// to start when a mounted route is not declared. Real AS2 partners do configure
// each other's URLs, so a configurable path is a genuine gap in the core
// contract rather than something to work around here. It has been reported.
//
// The default is the one every AS2 product's own examples use, so a partner
// pointed at http://host:8822/as2 needs no explanation.
const (
	pathReceive     = "/as2"
	pathCertificate = "/as2/certificate"
)

// Provider is the AS2 receiving endpoint.
type Provider struct {
	identity *as2.Identity

	// recv handles a message whose AS2-To is the one we expect - which is every
	// message when as2To is unset, the default.
	recv *as2.Receiver
	// mismatch is a second receiver, differing only in the metadata it stamps
	// on the event. It is nil unless as2To is pinned. Two receivers built once
	// beats building one per request: a Receiver is immutable after
	// construction and the only thing that varies is four bytes of metadata.
	mismatch *as2.Receiver

	// as2To is the identifier this endpoint answers to, or "" for "anything",
	// which is the default and what rule 1 asks for.
	as2To string

	deps plugin.Deps
}

// New returns the AS2 HTTP provider.
func New() *Provider { return &Provider{} }

// Name returns the provider's registry segment.
func (p *Provider) Name() string { return Name }

// Plugin says this provider belongs to the as2 plugin.
func (p *Provider) Plugin() string { return as2.Name }

// Description says what this stands in for.
func (p *Provider) Description() string {
	return "Stands in for a trading partner's AS2 endpoint (RFC 4130). Accepts an EDIINT message over " +
		"HTTP POST - signed, encrypted, compressed, in any combination - unwraps it, stores the EDI " +
		"document inside, and answers synchronously with a real MDN receipt. Serves its own certificate " +
		"as PEM so a partner can encrypt to it without anyone copying files around."
}

// BindIdentity is the as2.IdentityBinder half of the seam: the plugin hands
// every provider the shared key pair at construction time, so the provider is
// given a certificate without the plugin having to import it.
func (p *Provider) BindIdentity(id *as2.Identity) { p.identity = id }

// Endpoints declares the two routes RegisterIngress mounts, and only those.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method: "POST",
			Path:   pathReceive,
			Description: "Accept an AS2 message and answer with a synchronous MDN receipt. Signed, " +
				"encrypted and compressed messages are unwrapped; anything that cannot be opened is " +
				"still captured and reported honestly in the MDN's disposition rather than refused.",
		},
		{
			Method: "GET",
			Path:   pathCertificate,
			Description: "Serve tommy's AS2 certificate as PEM. This is where an exchange starts: a " +
				"partner has to import this before it can encrypt anything to tommy, and it is served " +
				"before any message has arrived.",
		},
	}
}

// Snippets are the whole product here: an AS2 exchange nobody has done before is
// mostly a certificate-swapping problem, and these two are the answer to "how do
// I test my AS2 integration locally".
//
// Both are executed against a live instance by TestSnippetsActuallyRun rather
// than merely rendered, because a snippet that looks right is worth nothing.
// That test has already earned it: the second snippet's first draft did not
// decrypt, and only running it said so. If one changes, the README beside this
// file has to change with it.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Post a plain AS2 message and read the MDN",
			Lang:  "bash",
			// The cold-start snippet: no keys, no setup, nothing fetched. An
			// unsigned, unencrypted AS2 message is legal (RFC 4130 §2.4 makes
			// signing and encryption optional) and gets back a real unsigned
			// MDN, so the first thing anyone runs works.
			Code: `printf 'ISA*00*          *00*          *ZZ*PARTNER        *ZZ*TOMMY          *260903*1200*U*00401*000000001*0*P*>~SE*1*0001~IEA*1*000000001~' |
curl -s -D - --data-binary @- \
  -H 'AS2-From: PARTNER' \
  -H 'AS2-To: TOMMY' \
  -H 'AS2-Version: 1.1' \
  -H 'Message-ID: <1@partner.example>' \
  -H 'Content-Type: application/edi-x12' \
  -H 'Disposition-Notification-To: as2@partner.example' \
  {{.IngressURL}}/as2`,
		},
		{
			Title: "Sign and encrypt with OpenSSL, then verify the signed MDN",
			Lang:  "bash",
			// The valuable one. It is a complete AS2 exchange driven entirely by
			// OpenSSL: fetch the receiver's certificate, build a throwaway
			// partner key pair, sign, encrypt, post, and verify the receipt came
			// back signed by the certificate fetched in step 1.
			Code: `# 1. Fetch tommy's certificate. Every AS2 relationship starts here.
curl -s -o tommy.pem {{.IngressURL}}/as2/certificate

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
  {{.IngressURL}}/as2

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
grep -E 'Disposition|Received-Content-MIC' mdn.report`,
		},
	}
}

// RegisterIngress configures the identity and mounts the two routes.
//
// This is the only place a provider is handed both a ProviderConfig and a
// ConfigDir - RegisterAPI and RegisterUI get Deps with an empty Config by
// contract - so it is where the identity has to be configured. That is also
// what keeps certificate generation lazy in the right way: nothing is generated
// for a provider that is switched off, so somebody running `tommy mail` never
// meets an AS2 certificate.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize().WithLogger("plugin", as2.Name, "provider", Name)
	p.deps = d

	if p.identity == nil {
		// Only reachable when a provider is mounted outside the plugin, which
		// the conformance harness does. Give it an identity of its own rather
		// than nil-panicking on the first request.
		p.identity = as2.NewIdentity()
	}
	if err := p.identity.Configure(as2.IdentityConfig{
		CertFile:        d.Config.String("cert_file", ""),
		KeyFile:         d.Config.String("key_file", ""),
		PartnerCertFile: d.Config.String("partner_cert_file", ""),
		Dir:             d.Config.String("cert_dir", ""),
		CommonName:      d.Config.String("common_name", ""),
		InMemory:        d.Config.Bool("in_memory", false),
		ConfigDir:       d.ConfigDir,
	}); err != nil {
		// Carry on deliberately. A broken certificate path must not take the
		// endpoint down: messages are still captured and shown, they simply
		// cannot be decrypted, and the MDN says exactly that. Refusing to
		// start would hide the one thing the operator needs to see.
		d.Logger.Warn("as2: identity not configured; messages will be captured but not decrypted", "err", err)
	}

	opts := []as2.ReceiverOption{as2.WithProvider(Name)}
	if n := d.Config.Int("max_body", 0); n > 0 {
		opts = append(opts, as2.WithMaxBody(int64(n)))
	}

	p.as2To = d.Config.String("as2_to", "")
	if p.as2To != "" {
		p.recv = as2.NewReceiver(p.identity, d, append(opts, as2.WithMeta(map[string]any{
			"as2_to_expected": p.as2To,
			"as2_to_matched":  true,
		}))...)
		p.mismatch = as2.NewReceiver(p.identity, d, append(opts, as2.WithMeta(map[string]any{
			"as2_to_expected": p.as2To,
			"as2_to_matched":  false,
		}))...)
	} else {
		p.recv = as2.NewReceiver(p.identity, d, opts...)
	}

	mux.HandleFunc("POST "+pathReceive, p.receive)
	mux.HandleFunc("GET "+pathCertificate, p.certificate)
}

// receive is the AS2 endpoint. It is Receiver.Handle with one thing in front of
// it: the AS2-To check.
//
// What that check does NOT do is refuse. RFC 4130 §6.2 is explicit - "There is
// no required response to a client request containing invalid or unknown
// AS2-From or AS2-To header values. The receiving AS2 system MAY return an
// unsigned MDN with an explanation of the error, if the sending system requested
// an MDN." So there is no status code and no disposition the RFC obliges us to
// send, and answering an HTTP error would be inventing policy the standard
// declines to set. What a capture tool can usefully do is *show* the mismatch,
// which is what the metadata is for: somebody who has misconfigured their
// AS2-To sees it on the message in the tab instead of watching a green light.
//
// Comparison is byte-exact on purpose. RFC 4130 §6.2 makes AS2 identifiers
// case-sensitive, so "TOMMY" and "tommy" are two different partners and folding
// them together here would hide exactly the misconfiguration this setting is for.
func (p *Provider) receive(w http.ResponseWriter, r *http.Request) {
	recv := p.recv
	if p.mismatch != nil && r.Header.Get("AS2-To") != p.as2To {
		p.deps.Logger.Warn("as2: message addressed to another identifier",
			"as2_to", r.Header.Get("AS2-To"), "expected", p.as2To)
		recv = p.mismatch
	}
	recv.Handle(w, r)
}

// certificate serves tommy's certificate as PEM.
//
// This endpoint is why the encrypted flow has a cold start at all: a partner
// cannot encrypt anything until it has this, and the alternative is telling
// somebody to find a file inside a container. It is on the ingress rather than
// only on the API so that the one URL a partner was given is enough - the
// plugin serves the same bytes at /api/v1/as2/certificate for the UI's benefit.
//
// It works before any message has arrived, because the identity is configured
// in RegisterIngress rather than on first use.
func (p *Provider) certificate(w http.ResponseWriter, r *http.Request) {
	pemBytes := p.identity.CertificatePEM()
	if len(pemBytes) == 0 {
		// 503 rather than the plugin API's 404, and the difference is real:
		// that route can be hit with no AS2 provider enabled at all, where
		// "there is no certificate" is a true and permanent answer. This route
		// only exists because this provider is enabled, so an absent
		// certificate means the configuration it was given is broken - a
		// condition the operator fixes and retries, which is what 503 says.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusServiceUnavailable)
		msg := "no AS2 certificate is available."
		if _, _, err := p.identity.KeyPair(); err != nil {
			msg += " " + err.Error()
		}
		fmt.Fprintln(w, "as2: "+msg+
			"\nCheck the provider's cert_file and key_file, or leave both unset to have one generated.")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="tommy-as2.pem"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pemBytes)
}

// interfaceGuards. A provider that quietly stopped satisfying IdentityBinder
// would still compile and would silently receive nothing it could decrypt.
var (
	_ plugin.Provider    = (*Provider)(nil)
	_ as2.IdentityBinder = (*Provider)(nil)
)
