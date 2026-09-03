// cmd/as2_test.go is cmd/as2.go's counterpart of the flag-mapping tests in
// cmd/singleplugin_test.go: it lives in its own file, rather than folded
// into that shared one, since this task's ownership is scoped to the files
// it created (cmd/as2.go, tommy.toml, README.md) plus whatever test file the
// new command needs.
package cmd

import (
	"testing"

	"github.com/can3p/tommy/plugins/as2/providers/http"
	"github.com/spf13/cobra"
)

// TestAS2HTTPFlagsLandInRightSection is the as2 equivalent of
// TestMailjetAndSendgridFlagsLandInRightSection: every one of the eight
// as2/http settings, pinned through its flag, must land in the http
// provider's section under the TOML key the provider actually reads
// (plugins/as2/providers/http/http.go's RegisterIngress), with the right Go
// type - a bool landing as a bool, an int as an int, never a string.
func TestAS2HTTPFlagsLandInRightSection(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var f as2HTTPOptionFlags
	registerAS2HTTPOptionFlags(cmd, &f)

	if err := cmd.ParseFlags([]string{
		"--as2-cert-file", "/tmp/cert.pem",
		"--as2-key-file", "/tmp/key.pem",
		"--as2-partner-cert-file", "/tmp/partner.pem",
		"--as2-cert-dir", "/tmp/certs",
		"--as2-common-name", "tommy.test",
		"--as2-in-memory",
		"--as2-to", "TOMMY",
		"--as2-max-body", "1024",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(http.Name, "as2-cert-file", "cert_file", f.certFile)
	opts.set(http.Name, "as2-key-file", "key_file", f.keyFile)
	opts.set(http.Name, "as2-partner-cert-file", "partner_cert_file", f.partnerCertFile)
	opts.set(http.Name, "as2-cert-dir", "cert_dir", f.certDir)
	opts.set(http.Name, "as2-common-name", "common_name", f.commonName)
	opts.set(http.Name, "as2-in-memory", "in_memory", f.inMemory)
	opts.set(http.Name, "as2-to", "as2_to", f.as2To)
	opts.set(http.Name, "as2-max-body", "max_body", f.maxBody)

	httpOpts, ok := opts.options["http"]
	if !ok {
		t.Fatal("no options recorded for http")
	}
	if len(httpOpts) != 8 {
		t.Errorf("http options = %+v, want exactly all eight settings", httpOpts)
	}
	want := map[string]any{
		"cert_file":         "/tmp/cert.pem",
		"key_file":          "/tmp/key.pem",
		"partner_cert_file": "/tmp/partner.pem",
		"cert_dir":          "/tmp/certs",
		"common_name":       "tommy.test",
		"in_memory":         true,
		"as2_to":            "TOMMY",
		"max_body":          1024,
	}
	for k, v := range want {
		if httpOpts[k] != v {
			t.Errorf("http options[%q] = %v (%T), want %v (%T)", k, httpOpts[k], httpOpts[k], v, v)
		}
	}

	cfg, err := singlePluginConfig("as2", []string{"http"}, baseSinglePluginFlags(), opts.options)
	if err != nil {
		t.Fatalf("singlePluginConfig: %v", err)
	}
	httpCfg := cfg.Provider("as2", "http")
	if got := httpCfg.String("as2_to", ""); got != "TOMMY" {
		t.Errorf("as2_to = %q, want TOMMY", got)
	}
	if got := httpCfg.Bool("in_memory", false); !got {
		t.Error("in_memory should be true")
	}
	if got := httpCfg.Int("max_body", 0); got != 1024 {
		t.Errorf("max_body = %d, want 1024", got)
	}
}

// TestAS2HTTPUnsetFlagsLeaveProviderDefaultsAlone checks that leaving every
// as2/http flag unset produces no overrides at all - in particular that
// --as2-in-memory's bool flag, whose zero value is false and whose provider
// default is also false, is not mistaken for "the operator asked for
// in_memory=false" the way an always-present int or string default could be.
// newProviderOptionBuilder.set only records a key when its flag was actually
// Changed(), so an untouched bool flag must be absent from the section
// entirely, not merely equal to its default.
func TestAS2HTTPUnsetFlagsLeaveProviderDefaultsAlone(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var f as2HTTPOptionFlags
	registerAS2HTTPOptionFlags(cmd, &f)
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(http.Name, "as2-cert-file", "cert_file", f.certFile)
	opts.set(http.Name, "as2-key-file", "key_file", f.keyFile)
	opts.set(http.Name, "as2-partner-cert-file", "partner_cert_file", f.partnerCertFile)
	opts.set(http.Name, "as2-cert-dir", "cert_dir", f.certDir)
	opts.set(http.Name, "as2-common-name", "common_name", f.commonName)
	opts.set(http.Name, "as2-in-memory", "in_memory", f.inMemory)
	opts.set(http.Name, "as2-to", "as2_to", f.as2To)
	opts.set(http.Name, "as2-max-body", "max_body", f.maxBody)

	if httpOpts, ok := opts.options["http"]; ok {
		t.Errorf("http options = %+v, want none recorded when no flag was set", httpOpts)
	}
}

// TestAS2ProvidersMatchAllAS2 checks as2Providers stays in sync with
// plugins/all/all.go's wiring - the same guard every other single-plugin
// shortcut's test suite would apply if it had one; as2 ships only one
// provider today, so this just pins that it is the http one.
func TestAS2ProvidersMatchAllAS2(t *testing.T) {
	providers := as2Providers()
	if len(providers) != 1 {
		t.Fatalf("as2Providers() = %v, want exactly one provider", providerNames(providers))
	}
	if providers[0].Name() != http.Name {
		t.Errorf("as2Providers()[0].Name() = %q, want %q", providers[0].Name(), http.Name)
	}
}
