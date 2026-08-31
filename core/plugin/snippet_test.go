package plugin_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin"
)

func TestNewSnippetCtx(t *testing.T) {
	tests := []struct {
		name                                string
		host, ui, api, in                   string
		wantUI, wantAPI, wantIngress, wPort string
	}{
		{
			name: "loopback",
			host: "localhost", ui: "127.0.0.1:8811", api: "127.0.0.1:8811", in: "127.0.0.1:8822",
			wantUI: "http://127.0.0.1:8811", wantAPI: "http://127.0.0.1:8811/api/v1",
			wantIngress: "http://127.0.0.1:8822", wPort: "8822",
		},
		{
			// A wildcard bind is useless in a copy-pasted command, so it is
			// replaced by the display host.
			name: "wildcard bind uses the display host",
			host: "tommy.local", ui: "0.0.0.0:8811", api: "0.0.0.0:8811", in: "0.0.0.0:9000",
			wantUI: "http://tommy.local:8811", wantAPI: "http://tommy.local:8811/api/v1",
			wantIngress: "http://tommy.local:9000", wPort: "9000",
		},
		{
			name: "ephemeral ports are carried through",
			host: "localhost", ui: "127.0.0.1:54321", api: "127.0.0.1:54321", in: "127.0.0.1:54322",
			wantUI: "http://127.0.0.1:54321", wantAPI: "http://127.0.0.1:54321/api/v1",
			wantIngress: "http://127.0.0.1:54322", wPort: "54322",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := plugin.NewSnippetCtx(tc.host, tc.ui, tc.api, tc.in)
			if ctx.UIURL != tc.wantUI {
				t.Errorf("UIURL = %q, want %q", ctx.UIURL, tc.wantUI)
			}
			if ctx.APIURL != tc.wantAPI {
				t.Errorf("APIURL = %q, want %q", ctx.APIURL, tc.wantAPI)
			}
			if ctx.IngressURL != tc.wantIngress {
				t.Errorf("IngressURL = %q, want %q", ctx.IngressURL, tc.wantIngress)
			}
			if ctx.IngressPort() != tc.wPort {
				t.Errorf("IngressPort = %q, want %q", ctx.IngressPort(), tc.wPort)
			}
		})
	}
}

func TestSetAddrFillsTheWellKnownFields(t *testing.T) {
	ctx := plugin.NewSnippetCtx("localhost", "127.0.0.1:1", "127.0.0.1:1", "127.0.0.1:2")
	ctx.SetAddr("mail", "smtp", "localhost:1025")
	ctx.SetAddr("files", "ftp", "localhost:2121")
	ctx.SetAddr("files", "sftp", "localhost:2222")
	ctx.SetAddr("hl7", "mllp", "localhost:2575")

	if ctx.SMTPAddr != "localhost:1025" {
		t.Errorf("SMTPAddr = %q", ctx.SMTPAddr)
	}
	if ctx.FTPAddr != "localhost:2121" || ctx.SFTPAddr != "localhost:2222" {
		t.Errorf("FTP/SFTP addrs = %q %q", ctx.FTPAddr, ctx.SFTPAddr)
	}
	// A protocol nobody thought of when SnippetCtx was written still resolves,
	// which is the point of the Addrs map.
	if got := ctx.Addr("hl7", "mllp"); got != "localhost:2575" {
		t.Errorf("Addr(hl7, mllp) = %q", got)
	}
	if got := ctx.Port("hl7", "mllp"); got != "2575" {
		t.Errorf("Port(hl7, mllp) = %q", got)
	}
	if got := ctx.Addr("nope", "nope"); got != "" {
		t.Errorf("Addr of an unknown provider = %q, want empty", got)
	}
	if got := ctx.Port("nope", "nope"); got != "" {
		t.Errorf("Port of an unknown provider = %q, want empty", got)
	}
}

func TestSnippetRender(t *testing.T) {
	ctx := plugin.NewSnippetCtx("localhost", "127.0.0.1:8811", "127.0.0.1:8811", "127.0.0.1:8822")
	ctx.SetAddr("mail", "smtp", "localhost:1025")

	tests := []struct {
		name    string
		code    string
		want    string
		wantErr string
	}{
		{
			name: "ingress url",
			code: "curl {{.IngressURL}}/v3.1/send",
			want: "curl http://127.0.0.1:8822/v3.1/send",
		},
		{
			name: "api and ui urls",
			code: "{{.APIURL}} {{.UIURL}} {{.Host}}",
			want: "http://127.0.0.1:8811/api/v1 http://127.0.0.1:8811 localhost",
		},
		{
			name: "listener address",
			code: "swaks --server {{.SMTPAddr}}",
			want: "swaks --server localhost:1025",
		},
		{
			name: "addr helper",
			code: `nc {{.Host}} {{.Port "mail" "smtp"}}`,
			want: "nc localhost 1025",
		},
		{
			name:    "unknown field is an error, not a silent blank",
			code:    "{{.NoSuchField}}",
			wantErr: "render",
		},
		{
			name:    "unparseable template",
			code:    "{{.IngressURL",
			wantErr: "parse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := plugin.Snippet{Title: tc.name, Lang: "bash", Code: tc.code}
			got, err := s.Render(ctx)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.name) {
					t.Errorf("error = %v, want it to name the snippet", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderSnippets(t *testing.T) {
	ctx := plugin.NewSnippetCtx("localhost", "127.0.0.1:1", "127.0.0.1:1", "127.0.0.1:2")
	got, err := plugin.RenderSnippets([]plugin.Snippet{
		{Title: "a", Lang: "bash", Code: "one {{.Host}}"},
		{Title: "b", Lang: "go", Code: "two"},
	}, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(got) != 2 || got[0].Code != "one localhost" || got[1].Lang != "go" {
		t.Errorf("got %+v", got)
	}

	if _, err := plugin.RenderSnippets([]plugin.Snippet{{Title: "bad", Code: "{{.Nope}}"}}, ctx); err == nil {
		t.Error("a bad snippet must fail the whole batch rather than render half of it")
	}
}
