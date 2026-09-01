//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"io"
	"testing"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/all"
)

// startTommy boots a real tommy, every shipped plugin and provider included,
// on ephemeral ports - exactly what an application under test would run,
// wired the way docs/clients.md tells an SDK to point at it.
func startTommy(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, all.Plugins()...)
}

// waitForSMTPAddr polls until the SMTP listener has bound and published its
// address. It runs in its own goroutine (core/server.Start starts every
// ListenerProvider concurrently), so it is not guaranteed to be ready the
// instant Start returns.
func waitForSMTPAddr(t *testing.T, inst *testutil.Instance, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if addr := inst.Server.SnippetCtx().SMTPAddr; addr != "" {
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("smtp listener address never resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// base64Encode is the shorthand every SDK's own attachment field wants: a
// base64-encoded string, not raw bytes.
func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// readBlob reads back an attachment's bytes straight from tommy's blob
// store - the same store the provider that captured it wrote to - so a test
// can assert on exactly what was persisted rather than trusting the request
// it sent.
func readBlob(t *testing.T, inst *testutil.Instance, id string) (string, blob.Ref) {
	t.Helper()
	rc, ref, err := inst.Blobs.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("open blob %s: %v", id, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob %s: %v", id, err)
	}
	return string(data), ref
}
