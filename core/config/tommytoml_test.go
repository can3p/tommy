package config_test

import (
	"path/filepath"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/all"
)

// repoConfig is the checked-in example configuration, which opens by claiming
// that "every value below already matches tommy's built-in default, so an
// untouched copy of this file boots identically to plain `tommy serve`".
const repoConfig = "../../tommy.toml"

// TestRepoConfigIsDefaultEquivalent holds that claim to the code.
//
// Nothing checked it until the container image started shipping this exact file
// at /etc/tommy/tommy.toml and naming it in the default command, which turns a
// courtesy to readers into behavior: `docker run can3p/tommy` must give a
// stranger the same tommy the bare binary does, and a value in this file that
// drifted from its default would quietly narrow that.
//
// The comparison is behavioral rather than structural. A bare Config has no
// [plugins] sections at all, while this file spells out `enabled = true` and
// `port = 1025` for everything, so the two structs are never equal - what has
// to match is what they resolve to: the same listeners, the same retention, the
// same plugins and providers enabled, and the same port under each listener
// provider.
func TestRepoConfigIsDefaultEquivalent(t *testing.T) {
	fromFile, err := config.Load(repoConfig)
	if err != nil {
		t.Fatalf("load %s: %v", filepath.Clean(repoConfig), err)
	}
	fromFile.ApplyDefaults()
	if err := fromFile.Validate(); err != nil {
		t.Fatalf("%s does not validate: %v", repoConfig, err)
	}

	defaults := config.Default()

	type check struct {
		what string
		got  any
		want any
	}
	checks := []check{
		{"bind", fromFile.Bind, defaults.Bind},
		{"host", fromFile.Host, defaults.Host},
		{"ui listener", fromFile.UIAddr(), defaults.UIAddr()},
		{"api listener", fromFile.APIAddr(), defaults.APIAddr()},
		{"ingress listener", fromFile.IngressAddr(), defaults.IngressAddr()},
		{"api shares the ui listener", fromFile.APISharesUIListener(), defaults.APISharesUIListener()},
		{"ingress shares the ui listener", fromFile.IngressSharesUIListener(), defaults.IngressSharesUIListener()},
		{"h2c on the ui listener", fromFile.H2C("ui"), defaults.H2C("ui")},
		{"h2c on the api listener", fromFile.H2C("api"), defaults.H2C("api")},
		{"h2c on the ingress listener", fromFile.H2C("ingress"), defaults.H2C("ingress")},
		{"storage capacity", fromFile.Storage.Capacity, defaults.Storage.Capacity},
		{"storage blob limit", fromFile.Storage.BlobLimit, defaults.Storage.BlobLimit},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: %s has %v, a default tommy has %v", repoConfig, c.what, c.got, c.want)
		}
	}

	// What the file enables, and where each listener provider would bind: both
	// asked of the real shipped plugin set, since that is what a `docker run`
	// with this file mounted actually starts. Nothing binds - Registry.ListenPort
	// reports the configured-or-default port without starting a listener.
	fileReg, err := plugin.New(fromFile, all.Plugins()...)
	if err != nil {
		t.Fatalf("registry from %s: %v", repoConfig, err)
	}
	defaultReg, err := plugin.New(defaults, all.Plugins()...)
	if err != nil {
		t.Fatalf("registry from defaults: %v", err)
	}

	for _, p := range all.Plugins() {
		name := p.Name()
		if got, want := fromFile.PluginEnabled(name), defaults.PluginEnabled(name); got != want {
			t.Errorf("%s: plugin %s enabled=%v, a default tommy has %v", repoConfig, name, got, want)
		}
		if got, want := fromFile.CapacityFor(name), defaults.CapacityFor(name); got != want {
			t.Errorf("%s: plugin %s retains %d events, a default tommy retains %d", repoConfig, name, got, want)
		}
		for _, prov := range p.Providers() {
			ref := name + "/" + prov.Name()
			if got, want := fromFile.ProviderEnabled(name, prov.Name()), defaults.ProviderEnabled(name, prov.Name()); got != want {
				t.Errorf("%s: provider %s enabled=%v, a default tommy has %v", repoConfig, ref, got, want)
			}
			gotPort, gotOK := fileReg.ListenPort(name, prov.Name())
			wantPort, wantOK := defaultReg.ListenPort(name, prov.Name())
			if gotOK != wantOK || gotPort != wantPort {
				t.Errorf("%s: provider %s would listen on %v, a default tommy on %v", repoConfig, ref, gotPort, wantPort)
			}
		}
	}
}

// TestRepoConfigInheritsBindIntoListenerProviders is the container's half of
// the same claim, and the reason `--bind 0.0.0.0` is safe to put in the image's
// default command in front of a config the user may have copied from this file.
//
// A listener provider reads its own interface from its own section. This file
// leaves every one of those commented out, so all seven inherit the top-level
// bind - which the flag overwrites. If a future edit uncommented one of them,
// that provider would stay on loopback inside a container while its port was
// published, which is exactly the bug report this test exists to prevent.
func TestRepoConfigInheritsBindIntoListenerProviders(t *testing.T) {
	cfg, err := config.Load(repoConfig)
	if err != nil {
		t.Fatalf("load %s: %v", repoConfig, err)
	}
	cfg.Bind = "0.0.0.0" // what `tommy serve --bind 0.0.0.0` does to it
	cfg.ApplyDefaults()

	reg, err := plugin.New(cfg, all.Plugins()...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	for _, ref := range reg.ListenerRefs() {
		pc := reg.ProviderConfig(ref.Plugin.Name(), ref.Provider.Name())
		if got := pc.String("bind", "127.0.0.1"); got != "0.0.0.0" {
			t.Errorf("%s/%s would bind %s, not the configured 0.0.0.0: a published container port would never reach it",
				ref.Plugin.Name(), ref.Provider.Name(), got)
		}
	}
}
