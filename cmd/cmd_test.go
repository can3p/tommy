package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/plugins/all"
	"github.com/can3p/tommy/plugins/s3"
	s3http "github.com/can3p/tommy/plugins/s3/providers/http"
)

func resetFlags(t *testing.T) {
	t.Helper()
	before := serveFlags
	t.Cleanup(func() { serveFlags = before })
	serveFlags.configPath = ""
	serveFlags.uiPort = -1
	serveFlags.apiPort = -1
	serveFlags.ingressPort = -1
	serveFlags.bind = ""
	serveFlags.host = ""
	serveFlags.logLevel = "info"
	serveFlags.persist = ""
	serveFlags.storage = nil
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tommy.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	value, set := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if set {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestLoadConfigDefaults(t *testing.T) {
	resetFlags(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *cfg.UI.Port != 8811 || *cfg.Ingress.Port != 8822 {
		t.Errorf("ports = %d/%d", *cfg.UI.Port, *cfg.Ingress.Port)
	}
	if !cfg.APISharesUIListener() {
		t.Error("the API should share the UI listener by default")
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	resetFlags(t)
	serveFlags.configPath = writeConfig(t, "[ui]\nport = 9101\n[ingress]\nport = 9102\n[storage]\ncapacity = 7\n")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *cfg.UI.Port != 9101 || *cfg.Ingress.Port != 9102 || cfg.Storage.Capacity != 7 {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Source == "" {
		t.Error("the config should remember where it came from")
	}
}

func TestFlagsOverrideTheConfigFile(t *testing.T) {
	resetFlags(t)
	serveFlags.configPath = writeConfig(t, "[ui]\nport = 9101\n[ingress]\nport = 9102\n")
	serveFlags.uiPort = 7000
	serveFlags.ingressPort = 7001
	serveFlags.host = "tommy.test"

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *cfg.UI.Port != 7000 || *cfg.Ingress.Port != 7001 {
		t.Errorf("flags did not win: %d/%d", *cfg.UI.Port, *cfg.Ingress.Port)
	}
	if cfg.Host != "tommy.test" {
		t.Errorf("host = %q", cfg.Host)
	}
}

func TestS3BucketsEnvironmentOverridesTOML(t *testing.T) {
	resetFlags(t)
	t.Setenv(s3BucketsEnv, "media, exports")
	serveFlags.configPath = writeConfig(t, "[plugins.s3.providers.http]\nbuckets = [\"from-file\"]\n")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s3http.LoadConfig(cfg.Provider(s3.PluginName, s3http.ProviderName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded.Buckets, ","); got != "media,exports" {
		t.Fatalf("buckets = %q, want environment value", got)
	}
}

func TestS3BucketsTOMLSurvivesWithoutEnvironmentOverride(t *testing.T) {
	resetFlags(t)
	unsetEnv(t, s3BucketsEnv)
	serveFlags.configPath = writeConfig(t, "[plugins.s3.providers.http]\nbuckets = [\"from-file\"]\n")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s3http.LoadConfig(cfg.Provider(s3.PluginName, s3http.ProviderName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded.Buckets, ","); got != "from-file" {
		t.Fatalf("buckets = %q, want TOML value", got)
	}
}

func TestEmptyS3BucketsEnvironmentClearsTOML(t *testing.T) {
	resetFlags(t)
	t.Setenv(s3BucketsEnv, "")
	serveFlags.configPath = writeConfig(t, "[plugins.s3.providers.http]\nbuckets = [\"from-file\"]\n")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s3http.LoadConfig(cfg.Provider(s3.PluginName, s3http.ProviderName))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Buckets) != 0 {
		t.Fatalf("buckets = %q, want explicit empty override", loaded.Buckets)
	}
}

// TestPersistFlagSetsFilesystemStorage checks that --persist is shorthand for
// storage.backend = "filesystem" plus storage.path = PATH.
func TestPersistFlagSetsFilesystemStorage(t *testing.T) {
	resetFlags(t)
	serveFlags.persist = "/data/tommy"

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Storage.Backend != config.StorageFilesystem {
		t.Errorf("backend = %q, want %q", cfg.Storage.Backend, config.StorageFilesystem)
	}
	if cfg.Storage.Path != "/data/tommy" {
		t.Errorf("path = %q, want /data/tommy", cfg.Storage.Path)
	}
}

// TestPersistEnvOverlaysServe checks that TOMMY_PERSIST does what --persist
// does when the flag itself was never given.
func TestPersistEnvOverlaysServe(t *testing.T) {
	resetFlags(t)
	t.Setenv(persistEnv, "/data/from-env")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Storage.Backend != config.StorageFilesystem || cfg.Storage.Path != "/data/from-env" {
		t.Errorf("storage = %+v, want filesystem at /data/from-env", cfg.Storage)
	}
}

// TestPersistFlagBeatsEnv checks the documented precedence: an explicit
// --persist wins over TOMMY_PERSIST.
func TestPersistFlagBeatsEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv(persistEnv, "/data/from-env")
	serveFlags.persist = "/data/from-flag"

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Storage.Path != "/data/from-flag" {
		t.Errorf("path = %q, want the flag's value to win over the environment", cfg.Storage.Path)
	}
}

// TestStorageTOMLRetainedWithoutPersistOrEnv checks that neither --persist
// nor TOMMY_PERSIST clobbers a TOML file's own [storage] when neither is
// given.
func TestStorageTOMLRetainedWithoutPersistOrEnv(t *testing.T) {
	resetFlags(t)
	unsetEnv(t, persistEnv)
	serveFlags.configPath = writeConfig(t, "[storage]\nbackend = \"filesystem\"\npath = \"from-toml\"\n")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Storage.Backend != config.StorageFilesystem || cfg.Storage.Path != "from-toml" {
		t.Errorf("storage = %+v, want the TOML file's own filesystem/from-toml untouched", cfg.Storage)
	}
}

// TestStorageFlagOverridesScope checks that --storage s3=memory combined with
// --persist produces a global filesystem backend with a plugin-scoped
// override back to memory for s3 - the combination the task description
// calls out explicitly.
func TestStorageFlagOverridesScope(t *testing.T) {
	resetFlags(t)
	serveFlags.persist = "/data/tommy"
	serveFlags.storage = []string{"s3=memory"}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Storage.Backend != config.StorageFilesystem {
		t.Errorf("global backend = %q, want filesystem from --persist", cfg.Storage.Backend)
	}
	if got := cfg.Storage.Plugins["s3"].Backend; got != config.StorageMemory {
		t.Errorf("s3 override = %q, want memory", got)
	}
	if got := cfg.Storage.BackendFor("s3", ""); got != config.StorageMemory {
		t.Errorf("BackendFor(s3) = %q, want memory", got)
	}
	if got := cfg.Storage.BackendFor("files", ""); got != config.StorageFilesystem {
		t.Errorf("BackendFor(files) = %q, want filesystem (the global default)", got)
	}
}

// TestStorageFlagMalformedErrors checks that a --storage value with no '='
// is rejected with a clear error naming the flag, rather than silently doing
// nothing or panicking.
func TestStorageFlagMalformedErrors(t *testing.T) {
	resetFlags(t)
	serveFlags.storage = []string{"s3-memory"}

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected an error for a malformed --storage value")
	}
	if !strings.Contains(err.Error(), "--storage") || !strings.Contains(err.Error(), "s3-memory") {
		t.Errorf("err = %v, want it to name --storage and the bad value", err)
	}
}

// TestStorageFlagUnknownScopeErrors checks that an invalid scope shape (more
// than one '/') is reported, proving the CLI actually reaches
// config.SetStorageOverride rather than swallowing its error.
func TestStorageFlagUnknownScopeErrors(t *testing.T) {
	resetFlags(t)
	serveFlags.storage = []string{"a/b/c=filesystem"}

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected an error for an invalid override scope")
	}
}

// TestPersistRelativePathStaysCwdRelative checks that a relative --persist
// path is left alone when the config was never loaded from a file - see
// Config.StoragePath, which only resolves a relative path against a config
// file's own directory.
// A relative path typed on the command line is relative to where the command
// ran, even beside --config: only a path written in the file is read relative
// to the file.
func TestPersistRelativePathIsWorkingDirectoryRelative(t *testing.T) {
	resetFlags(t)
	serveFlags.configPath = writeConfig(t, "")
	serveFlags.persist = "relative/tommy-data"

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want, _ := filepath.Abs("relative/tommy-data")
	if got := cfg.StoragePath(); got != want {
		t.Errorf("StoragePath() = %q, want %q (working-directory relative, not config-relative)", got, want)
	}
}

func TestLoadConfigReportsBadInput(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		resetFlags(t)
		serveFlags.configPath = filepath.Join(t.TempDir(), "nope.toml")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected an error for a missing config file")
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		resetFlags(t)
		serveFlags.configPath = writeConfig(t, "[ui]\nport = 70000\n")
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("port collision from flags", func(t *testing.T) {
		resetFlags(t)
		serveFlags.uiPort = 9000
		serveFlags.apiPort = 9001
		serveFlags.ingressPort = 9001
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected a port collision to be reported")
		}
	})
}

func TestNewLogger(t *testing.T) {
	resetFlags(t)
	if _, err := newLogger("debug"); err != nil {
		t.Errorf("debug: %v", err)
	}
	if _, err := newLogger("shout"); err == nil {
		t.Error("an unknown level must be rejected with a helpful message")
	}
}

// The command describes whatever this binary was compiled with, so the test
// asserts the shape of the output rather than a fixed roster that changes every
// time a plugin is added.
func TestProvidersCommandListsCompiledPlugins(t *testing.T) {
	resetFlags(t)
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	t.Cleanup(func() { providersCmd.SetOut(nil) })

	if err := providersCmd.RunE(providersCmd, nil); err != nil {
		t.Fatalf("providers: %v", err)
	}

	got := out.String()
	if len(all.Plugins()) == 0 {
		if !strings.Contains(got, "No plugins are enabled") {
			t.Errorf("with nothing compiled in, output = %q", got)
		}
		return
	}
	for _, p := range all.Plugins() {
		if !strings.Contains(got, p.Name()) {
			t.Errorf("output does not mention plugin %q: %q", p.Name(), got)
		}
		if !strings.Contains(got, p.Description()) {
			t.Errorf("output does not carry %q's description, which is the point of the command", p.Name())
		}
	}
}

func TestProvidersCommandJSON(t *testing.T) {
	resetFlags(t)
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	providersFlags.asJSON = true
	t.Cleanup(func() {
		providersCmd.SetOut(nil)
		providersFlags.asJSON = false
	})

	if err := providersCmd.RunE(providersCmd, nil); err != nil {
		t.Fatalf("providers: %v", err)
	}
	var got []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(got) != len(all.Plugins()) {
		t.Fatalf("described %d plugins, want the %d compiled in", len(got), len(all.Plugins()))
	}
	for _, p := range got {
		if p.Name == "" || p.Description == "" {
			t.Errorf("plugin %+v is missing a name or description", p)
		}
	}
}

func TestProvidersCommandUnknownName(t *testing.T) {
	resetFlags(t)
	providersCmd.SetOut(&bytes.Buffer{})
	t.Cleanup(func() { providersCmd.SetOut(nil) })

	err := providersCmd.RunE(providersCmd, []string{"nosuchplugin"})
	if err == nil || !strings.Contains(err.Error(), "nosuchplugin") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandsAreRegistered(t *testing.T) {
	want := map[string]bool{
		"serve": false, "providers": false,
		"mail": false, "sms": false, "files": false, "chat": false, "s3": false,
	}
	for _, c := range rootCmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("command %q is not registered on the root command", name)
		}
	}
}

// runProviders runs the command the way a user would and returns what it
// printed. It starts nothing: `tommy providers` must never bind a listener,
// which is also why this test can name well-known ports safely.
func runProviders(t *testing.T, asJSON bool, configPath string, args ...string) string {
	t.Helper()
	resetFlags(t)
	serveFlags.configPath = configPath
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	providersFlags.asJSON = asJSON
	t.Cleanup(func() {
		providersCmd.SetOut(nil)
		providersFlags.asJSON = false
	})
	if err := providersCmd.RunE(providersCmd, args); err != nil {
		t.Fatalf("providers: %v", err)
	}
	return out.String()
}

type listedProvider struct {
	Name     string `json:"name"`
	Listener bool   `json:"listener"`
	Addr     string `json:"addr"`
	Port     int    `json:"port"`
	Network  string `json:"network"`
}

type listedPlugin struct {
	Name      string           `json:"name"`
	Providers []listedProvider `json:"providers"`
}

func listedProviders(t *testing.T, body string) []listedPlugin {
	t.Helper()
	var got []listedPlugin
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, body)
	}
	return got
}

// TestProvidersReportsListenerPorts is the reason ListenPort exists: with no
// configuration at all, the listing has to say which ports a default tommy
// would bind. It used to report none, because it only knew a port the config
// named explicitly, and every default lived in a package constant instead.
func TestProvidersReportsListenerPorts(t *testing.T) {
	var listeners int
	for _, p := range listedProviders(t, runProviders(t, true, "")) {
		for _, prov := range p.Providers {
			if !prov.Listener {
				if prov.Port != 0 {
					t.Errorf("%s/%s is path-routed onto the shared ingress but reports port %d", p.Name, prov.Name, prov.Port)
				}
				continue
			}
			listeners++
			if prov.Port == 0 {
				t.Errorf("%s/%s reports no port; the default lives in a constant nothing can read", p.Name, prov.Name)
			}
			if prov.Network != "tcp" && prov.Network != "udp" {
				t.Errorf("%s/%s reports network %q", p.Name, prov.Name, prov.Network)
			}
			if prov.Addr == "" {
				t.Errorf("%s/%s reports no address for a snippet to render against", p.Name, prov.Name)
			}
		}
	}
	if listeners == 0 {
		t.Skip("this build has no listener providers compiled in")
	}
}

// TestProvidersHonoursAConfiguredPort proves the listing reports the resolved
// value rather than the constant, in both directions.
func TestProvidersHonoursAConfiguredPort(t *testing.T) {
	if !hasProvider(t, "mail", "smtp") {
		t.Skip("this build has no mail/smtp provider")
	}
	path := writeConfig(t, "[plugins.mail.providers.smtp]\nport = 9999\n")

	var found bool
	for _, p := range listedProviders(t, runProviders(t, true, path, "mail/smtp")) {
		for _, prov := range p.Providers {
			found = true
			if prov.Port != 9999 {
				t.Errorf("mail/smtp reports port %d, want the configured 9999", prov.Port)
			}
			if !strings.HasSuffix(prov.Addr, ":9999") {
				t.Errorf("mail/smtp reports addr %q, want the configured port", prov.Addr)
			}
		}
	}
	if !found {
		t.Fatal("mail/smtp was not listed")
	}

	// port = 0 is the ephemeral case: nothing can know it without binding, and
	// the listing must not invent the package default.
	ephemeral := writeConfig(t, "[plugins.mail.providers.smtp]\nport = 0\n")
	for _, p := range listedProviders(t, runProviders(t, true, ephemeral, "mail/smtp")) {
		for _, prov := range p.Providers {
			if prov.Port != 0 || prov.Addr != "" {
				t.Errorf("mail/smtp reports %+v for an ephemeral port; only a bound listener knows it", prov)
			}
		}
	}
}

// TestProvidersPrintsListenerPortsForHumans: the human form is what most people
// read, and it printed no port at all for a default run.
func TestProvidersPrintsListenerPortsForHumans(t *testing.T) {
	if !hasProvider(t, "mail", "smtp") {
		t.Skip("this build has no mail/smtp provider")
	}
	got := runProviders(t, false, "", "mail/smtp")
	if !strings.Contains(got, "own tcp listener on localhost:1025") {
		t.Errorf("output does not say where the listener would bind:\n%s", got)
	}
}

func hasProvider(t *testing.T, plugin, provider string) bool {
	t.Helper()
	for _, p := range all.Plugins() {
		if p.Name() != plugin {
			continue
		}
		for _, prov := range p.Providers() {
			if prov.Name() == provider {
				return true
			}
		}
	}
	return false
}
