// Package e2e_test runs whole-process, cross-plugin checks over
// core/testutil that no single plugin's own package tests can exercise:
// traffic through several real providers landing in one store, API
// isolation between plugins, an SSE stream carrying more than one plugin's
// events, a plugin-scoped DELETE, and the single-plugin CLI shortcuts
// (cmd/mail.go, cmd/sms.go) behaving identically to an equivalent TOML
// config file. Every server here binds ephemeral ports, so tests never
// collide and can run with -race -count=1 alongside everything else.
package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// tommyBinPath is the real tommy CLI, built once for the whole package.
// TestSinglePluginCLIMatchesConfigFile execs it rather than reimplementing
// cmd/mail.go's flag parsing and config building, which are unexported and
// meant to be exercised exactly as a user would invoke them.
var tommyBinPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tommy-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tommy e2e: mkdtemp:", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tommyBinPath = filepath.Join(dir, "tommy-e2e")
	if runtime.GOOS == "windows" {
		tommyBinPath += ".exe"
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "tommy e2e: cannot locate this test file to find the module root")
		os.Exit(1)
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")

	build := exec.Command("go", "build", "-o", tommyBinPath, ".")
	build.Dir = root
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tommy e2e: build tommy:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}
