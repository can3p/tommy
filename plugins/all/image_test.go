package all_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/all"
)

// The container image is a supported surface, and a surface nothing checks
// goes stale. A wave that adds a listener provider or moves a default port
// invalidates the Dockerfile and the compose file without breaking a single
// test - which is exactly how documentation drifted in this project before.
//
// These tests derive their expectation from the registry, the way
// ports_test.go does, so the image has to follow the binary rather than the
// other way round. Nothing binds; every provider is asked, none is started.

const (
	dockerfile  = "Dockerfile"
	composefile = "docker-compose.yml"
	repoConfig  = "tommy.toml"
	dockerHub   = "docs/dockerhub.md"
)

// Docker Hub truncates silently rather than refusing, so a page that grew past
// either cap would publish half a sentence and nobody would notice.
const (
	maxFullDescription  = 25_000
	maxShortDescription = 100
)

var shortDescription = regexp.MustCompile(`(?m)^<!--\s*short_description:\s*(.*?)\s*-->\s*$`)

// repoFile reads a file relative to the repository root. Walking up to the
// directory holding go.mod beats a chain of "../.." because it keeps working
// when a test moves package, and it fails loudly rather than reading nothing.
func repoFile(t *testing.T, rel string) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s, so the repository root cannot be found", dir)
		}
		dir = parent
	}
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// wantPorts is every port a default tommy listens on: each listener provider's
// own, from the registry, plus the two core listeners, from the config
// defaults. Neither number is typed here.
func wantPorts(t *testing.T) map[string]string {
	t.Helper()

	cfg := config.Default()
	reg, err := plugin.New(cfg, all.Plugins()...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	want := map[string]string{} // "2575/tcp" -> "hl7/mllp"
	for _, ref := range reg.ListenerRefs() {
		name := ref.Plugin.Name() + "/" + ref.Provider.Name()
		lp, ok := reg.ListenPort(ref.Plugin.Name(), ref.Provider.Name())
		if !ok || lp.Ephemeral() {
			// ports_test.go is the test that fails for this; here it would
			// only produce a confusing second failure.
			continue
		}
		want[lp.String()] = name
	}
	want[fmt.Sprintf("%d/tcp", *cfg.UI.Port)] = "ui and api"
	want[fmt.Sprintf("%d/tcp", *cfg.Ingress.Port)] = "ingress"
	return want
}

// TestDockerfileExposesEveryDefaultPort holds the image's EXPOSE list to the
// binary's own answer, in both directions: a listener the image does not
// publish is unreachable, and an EXPOSE line for a port nothing listens on is
// a lie in the other direction.
func TestDockerfileExposesEveryDefaultPort(t *testing.T) {
	want := wantPorts(t)

	// The Dockerfile deliberately keeps one port per line so this is a regexp
	// and not a parser.
	exposed := map[string]bool{}
	re := regexp.MustCompile(`(?m)^EXPOSE\s+(\d+)(?:/(tcp|udp))?\s*$`)
	for _, m := range re.FindAllStringSubmatch(repoFile(t, dockerfile), -1) {
		proto := m[2]
		if proto == "" {
			proto = "tcp" // Docker's own default
		}
		exposed[m[1]+"/"+proto] = true
	}
	if len(exposed) == 0 {
		t.Fatalf("no EXPOSE lines found in %s; this test parses one port per line", dockerfile)
	}

	for port, owner := range want {
		if !exposed[port] {
			t.Errorf("%s listens on %s and %s does not EXPOSE it: add `EXPOSE %s`, "+
				"and publish it in %s too", owner, port, dockerfile, port, composefile)
		}
	}
	for port := range exposed {
		if _, ok := want[port]; !ok {
			t.Errorf("%s exposes %s, which nothing listens on any more: remove the line, "+
				"or the image advertises a port that answers nothing", dockerfile, port)
		}
	}
}

// TestComposePublishesEveryDefaultPort is the same check for the compose file,
// which is what most people will actually run.
func TestComposePublishesEveryDefaultPort(t *testing.T) {
	want := wantPorts(t)
	compose := repoFile(t, composefile)

	// Only the container side of each mapping matters here: the host side is
	// the user's to change. A range ("50000-50009:50000-50009") is FTP's
	// passive data range - it belongs to no listener provider, because it is
	// where the *data* connection lands rather than where anything binds, so
	// it is deliberately not in want and is skipped rather than special-cased
	// by number.
	published := map[string]bool{}
	re := regexp.MustCompile(`(?m)^\s*-\s*"(\d+(?:-\d+)?):(\d+(?:-\d+)?)(?:/(tcp|udp))?"`)
	for _, m := range re.FindAllStringSubmatch(compose, -1) {
		if strings.Contains(m[2], "-") {
			continue // a range: see above
		}
		proto := m[3]
		if proto == "" {
			proto = "tcp"
		}
		published[m[2]+"/"+proto] = true
	}
	if len(published) == 0 {
		t.Fatalf("no published ports found in %s", composefile)
	}

	for port, owner := range want {
		if !published[port] {
			t.Errorf("%s listens on %s and %s does not publish it: add `- \"%s:%s\"` "+
				"(with /udp if it is udp)", owner, port, composefile,
				strings.SplitN(port, "/", 2)[0], strings.SplitN(port, "/", 2)[0])
		}
	}
	for port := range published {
		if _, ok := want[port]; !ok {
			t.Errorf("%s publishes %s, which nothing listens on any more", composefile, port)
		}
	}
}

// TestImageShipsTheRepositoryConfig holds the Dockerfile to copying the
// repository's own tommy.toml - the file core/config's TestRepoConfigIsDefault
// Equivalent proves is default-equivalent. A divergent copy would make that
// proof worthless.
//
// docker/tommy.toml is a different file on purpose: a tiny config the compose
// stack mounts *over* the image's copy, carrying only what a container
// genuinely changes. It must never become what the image ships.
func TestImageShipsTheRepositoryConfig(t *testing.T) {
	df := repoFile(t, dockerfile)

	re := regexp.MustCompile(`(?m)^COPY\s+(\S+)\s+/etc/tommy/tommy\.toml\s*$`)
	m := re.FindStringSubmatch(df)
	if m == nil {
		t.Fatalf("%s does not COPY anything to /etc/tommy/tommy.toml; the image's default "+
			"command names that path, so without it the container cannot start", dockerfile)
	}
	if m[1] != repoConfig {
		t.Errorf("%s ships %q at /etc/tommy/tommy.toml, but the image must ship the "+
			"repository's own %s - that is the file core/config proves default-equivalent. "+
			"docker/tommy.toml is the compose stack's mounted override, not the image's copy",
			dockerfile, m[1], repoConfig)
	}
}

// TestDockerHubPageFitsItsCaps measures bytes, not characters: an em dash is
// three bytes, and the cap Docker Hub applies is a byte cap.
//
// The other half of rule 14 for this page - that it stays a thin pointer
// rather than growing into a second README - is deliberately not tested. The
// obvious heuristic, counting how many plugins and providers it names, fires
// on the port table, where naming every listener is the point. A test that has
// to be worked around is worse than the judgement it replaces, so the reason
// the page is thin is written into the page instead.
func TestDockerHubPageFitsItsCaps(t *testing.T) {
	page := repoFile(t, dockerHub)

	if n := len(page); n > maxFullDescription {
		t.Errorf("%s is %d bytes, over Docker Hub's %d-byte cap: the page would be "+
			"truncated silently mid-sentence. It is meant to be a thin pointer page - "+
			"link to the site rather than growing it", dockerHub, n, maxFullDescription)
	}

	m := shortDescription.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("%s has no `<!-- short_description: ... -->` line; the release workflow "+
			"reads the repository's short description from there", dockerHub)
	}
	if n := len(m[1]); n > maxShortDescription {
		t.Errorf("the short description in %s is %d bytes, over Docker Hub's %d-byte cap "+
			"(it truncates silently): %q", dockerHub, n, maxShortDescription, m[1])
	}
}
