package mail_test

import (
	"context"
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/mailtest"
)

// start boots a whole tommy on ephemeral ports with the mail plugin and the
// test-only provider mounted, so every assertion runs against the real
// bootstrap rather than a hand-assembled subset.
func start(t *testing.T, providers ...plugin.Provider) *testutil.Instance {
	t.Helper()
	if len(providers) == 0 {
		providers = []plugin.Provider{mailtest.New()}
	}
	return testutil.Start(t, nil, mail.New(providers...))
}

func deps(in *testutil.Instance) plugin.Deps {
	return plugin.Deps{Store: in.Store, Blobs: in.Blobs}
}

// inject appends a message the way a provider would.
func inject(t *testing.T, in *testutil.Instance, msg *mail.Message, opts ...mailtest.Option) *event.Event {
	t.Helper()
	ev, err := mailtest.Inject(context.Background(), deps(in), msg, opts...)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	return ev
}

// sampleMessage is a two-part message with one attachment and one embedded
// image, which is the shape every real provider has to produce.
func sampleMessage(t *testing.T, in *testutil.Instance) *mail.Message {
	t.Helper()
	m := &mail.Message{
		From:    mail.Address{Name: "Alice", Email: "alice@example.com"},
		To:      []mail.Address{{Name: "Bob", Email: "bob@example.com"}},
		Cc:      []mail.Address{{Email: "carol@example.com"}},
		Bcc:     []mail.Address{{Email: "dan@example.com"}},
		ReplyTo: []mail.Address{{Email: "no-reply@example.com"}},
		Subject: "Invoice 42",
		Text:    "Please pay the attached invoice.",
		HTML:    `<p>Please pay the <b>attached</b> invoice.</p><img src="cid:logo@tommy">`,
	}
	m.Headers.Set("X-Campaign", "billing")
	if _, err := m.AttachBytes(context.Background(), in.Blobs, mail.Attachment{
		Filename:    "invoice.csv",
		ContentType: "text/csv",
	}, []byte("invoice,42\n")); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := m.AttachBytes(context.Background(), in.Blobs, mail.Attachment{
		Filename:    "logo.png",
		ContentType: "image/png",
		Inline:      true,
		ContentID:   "<logo@tommy>",
	}, []byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return m
}

func TestConformance(t *testing.T) {
	plugintest.Conformance(t, mail.New(mailtest.New()))
}

func TestPluginIdentity(t *testing.T) {
	t.Parallel()
	p := mail.New()
	if p.Name() != "mail" || p.Title() != "Mail" {
		t.Errorf("name/title = %q/%q", p.Name(), p.Title())
	}
	if len(p.Description()) < plugintest.MinDescription {
		t.Errorf("description is too short: %q", p.Description())
	}
	// Providers arrive in Wave 2; until then the plugin is still usable.
	if got := p.Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() = %v, want an empty non-nil slice", got)
	}
	if p.Templates() == nil {
		t.Error("Templates() must expose the tab's templates")
	}
}

// The tab is composed from the shared component library, so its templates only
// parse together with the components.
func TestTemplatesParseWithTheComponentLibrary(t *testing.T) {
	t.Parallel()
	tpl, err := ui.PluginTemplates(mail.New().Templates())
	if err != nil {
		t.Fatalf("PluginTemplates: %v", err)
	}
	for _, name := range []string{"mail-inbox", "mail-list", "mail-filter", "mail-pane", "mail-detail", "mail-body"} {
		if tpl.Lookup(name) == nil {
			t.Errorf("template %q is missing", name)
		}
	}
}

// A plugin with no providers at all must still boot, serve its API and render
// its tab: that is what Wave 1 ships before any real provider exists.
func TestPluginWithoutProvidersStillServes(t *testing.T) {
	in := testutil.Start(t, nil, mail.New())
	ev := inject(t, in, &mail.Message{
		From:    mail.Address{Email: "a@example.com"},
		To:      []mail.Address{{Email: "b@example.com"}},
		Subject: "No providers needed",
		Text:    "still works",
	}, mailtest.WithReceivedAt(time.Now()))

	var views []mail.MessageView
	if status := in.GetJSON(in.API("/mail/messages"), &views); status != 200 {
		t.Fatalf("list status = %d", status)
	}
	if len(views) != 1 || views[0].ID != ev.ID {
		t.Fatalf("list = %+v", views)
	}
	if status, _ := in.GetBody(in.UI("/mail/")); status != 200 {
		t.Fatalf("tab status = %d", status)
	}
}
