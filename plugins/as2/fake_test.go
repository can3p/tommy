package as2_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/as2"
)

// fakeProvider is a test-only AS2 provider. It exists because the HTTP provider
// is a separate task and the plugin core still has to be proven end to end -
// and because it is the smallest possible demonstration of the seam the real
// provider codes against: bind the identity, configure it in RegisterIngress,
// hand the route to Receiver.Handle.
//
// If this ever stops compiling, the seam changed and plugins/as2/README.md is
// out of date.
type fakeProvider struct {
	identity *as2.Identity
	recv     *as2.Receiver
}

func (p *fakeProvider) Name() string   { return "fake" }
func (p *fakeProvider) Plugin() string { return as2.Name }

func (p *fakeProvider) Description() string {
	return "A test-only AS2 endpoint that runs the plugin's own Receiver, so the as2 plugin core can be " +
		"exercised end to end before the real HTTP provider exists."
}

func (p *fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method:      "POST",
		Path:        "/fake-as2",
		Description: "Accept an AS2 message and answer with a synchronous MDN receipt.",
	}}
}

func (p *fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Post a plain AS2 message",
		Lang:  "bash",
		Code: `printf 'ISA*00*...IEA*1*000000001~' | curl -s -D - --data-binary @- \
  -H 'AS2-From: PARTNER' -H 'AS2-To: TOMMY' \
  -H 'Message-ID: <1@partner.example>' \
  -H 'Content-Type: application/edi-x12' \
  -H 'Disposition-Notification-To: as2@partner.example' \
  {{.IngressURL}}/fake-as2`,
	}}
}

// BindIdentity is the IdentityBinder half of the seam.
func (p *fakeProvider) BindIdentity(id *as2.Identity) { p.identity = id }

// RegisterIngress is the other half: it is the only place a provider has both a
// ProviderConfig and a ConfigDir, so it is where the identity is configured -
// and why nothing is generated for a provider that is switched off.
func (p *fakeProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	if p.identity == nil {
		p.identity = as2.NewIdentity()
	}
	_ = p.identity.Configure(as2.IdentityConfig{
		CertFile:        d.Config.String("cert_file", ""),
		KeyFile:         d.Config.String("key_file", ""),
		PartnerCertFile: d.Config.String("partner_cert_file", ""),
		Dir:             d.Config.String("cert_dir", ""),
		ConfigDir:       d.ConfigDir,
		// A test must never write to the user's real config directory.
		InMemory: d.Config.Bool("in_memory", true),
	})
	p.recv = as2.NewReceiver(p.identity, d, as2.WithProvider(p.Name()))
	mux.HandleFunc("POST /fake-as2", p.recv.Handle)
}

// start boots the real server with the as2 plugin and the fake provider, on
// ephemeral ports.
func start(t *testing.T, providerCfg map[string]any) *testutil.Instance {
	t.Helper()
	prov := &fakeProvider{}
	p := as2.New(prov)

	cfg := config.Ephemeral()
	if providerCfg != nil {
		cfg.SetProvider(as2.Name, "fake", config.NewProviderConfig(providerCfg))
	}
	return testutil.Start(t, cfg, p)
}

// startPlugin boots the real server with a plugin the caller built, on
// ephemeral ports. It exists for the case that matters most about laziness: a
// plugin with no provider at all.
func startPlugin(t *testing.T, p *as2.Plugin) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, config.Ephemeral(), p)
}

// startWithFixtureIdentity boots the server with the tommy.crt/tommy.key pair
// the fixtures were encrypted to, so an encrypted message can be driven all the
// way through the ingress.
func startWithFixtureIdentity(t *testing.T) *testutil.Instance {
	t.Helper()
	abs, err := filepath.Abs(testdataDir)
	if err != nil {
		t.Fatalf("resolve testdata: %v", err)
	}
	return start(t, map[string]any{
		"cert_file": filepath.Join(abs, "tommy.crt"),
		"key_file":  filepath.Join(abs, "tommy.key"),
	})
}

// post sends a fixture through the ingress the way an AS2 client would.
func post(t *testing.T, in *testutil.Instance, fixtureName string, extra http.Header) *http.Response {
	t.Helper()
	req := request(t, fixtureName, extra)
	httpReq, err := http.NewRequest(http.MethodPost, in.Ingress("/fake-as2"), bytesReader(req.Body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header = req.Header
	resp, err := in.Client.Do(httpReq)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}
