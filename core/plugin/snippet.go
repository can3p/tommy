package plugin

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"text/template"
)

// Snippet is a runnable example that puts real data into tommy from a cold
// start. Code is a Go template rendered against SnippetCtx, so a copied command
// always carries the ports this instance actually bound - a snippet with a
// hardcoded 8822 is wrong the moment someone passes --ingress-port.
type Snippet struct {
	Title string `json:"title"`
	Lang  string `json:"lang"` // "bash" | "go" | "python"
	Code  string `json:"code"`
}

// SnippetCtx carries the live runtime addresses a snippet renders against.
type SnippetCtx struct {
	Host       string `json:"host"`        // "localhost"
	IngressURL string `json:"ingress_url"` // "http://localhost:8822"
	UIURL      string `json:"ui_url"`      // "http://localhost:8811"
	APIURL     string `json:"api_url"`     // "http://localhost:8811/api/v1"
	SMTPAddr   string `json:"smtp_addr"`   // "localhost:1025"
	FTPAddr    string `json:"ftp_addr"`    // "localhost:2121"
	SFTPAddr   string `json:"sftp_addr"`   // "localhost:2222"

	// Addrs holds every listener provider's address keyed "<plugin>.<provider>",
	// so a provider added later does not need a new field here. Reach it from a
	// template with {{.Addr "files" "tftp"}}.
	Addrs map[string]string `json:"addrs,omitempty"`
}

// Addr returns the address of a listener provider, "" when it is not running.
func (c SnippetCtx) Addr(plugin, provider string) string {
	return c.Addrs[plugin+"."+provider]
}

// Port returns just the port of a listener provider, "" when it is not running.
func (c SnippetCtx) Port(plugin, provider string) string {
	_, port, err := net.SplitHostPort(c.Addr(plugin, provider))
	if err != nil {
		return ""
	}
	return port
}

// IngressPort returns the port part of IngressURL.
func (c SnippetCtx) IngressPort() string { return portOfURL(c.IngressURL) }

// UIPort returns the port part of UIURL.
func (c SnippetCtx) UIPort() string { return portOfURL(c.UIURL) }

func portOfURL(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return ""
	}
	return port
}

// SetAddr records a listener provider's address.
func (c *SnippetCtx) SetAddr(plugin, provider, addr string) {
	if c.Addrs == nil {
		c.Addrs = map[string]string{}
	}
	c.Addrs[plugin+"."+provider] = addr
	switch plugin + "." + provider {
	case "mail.smtp":
		c.SMTPAddr = addr
	case "files.ftp":
		c.FTPAddr = addr
	case "files.sftp":
		c.SFTPAddr = addr
	}
}

// NewSnippetCtx builds a context from resolved listener addresses.
func NewSnippetCtx(host, uiAddr, apiAddr, ingressAddr string) SnippetCtx {
	return SnippetCtx{
		Host:       host,
		UIURL:      httpURL(host, uiAddr),
		APIURL:     httpURL(host, apiAddr) + "/api/v1",
		IngressURL: httpURL(host, ingressAddr),
		Addrs:      map[string]string{},
	}
}

// httpURL turns a listen address into a URL a user can paste, replacing
// wildcard binds with the display host.
func httpURL(host, listenAddr string) string {
	h, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://" + listenAddr
	}
	if h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" {
		h = host
	}
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}
	if p, err := strconv.Atoi(port); err == nil && p == 80 {
		return "http://" + h
	}
	return "http://" + h + ":" + port
}

// Render renders the snippet against ctx.
func (s Snippet) Render(ctx SnippetCtx) (string, error) {
	tpl, err := template.New("snippet").Option("missingkey=error").Parse(s.Code)
	if err != nil {
		return "", fmt.Errorf("snippet %q: parse: %w", s.Title, err)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, ctx); err != nil {
		return "", fmt.Errorf("snippet %q: render: %w", s.Title, err)
	}
	return sb.String(), nil
}

// RenderedSnippet is a Snippet with its Code already rendered, which is what
// the API, the UI and the CLI all hand out.
type RenderedSnippet struct {
	Title string `json:"title"`
	Lang  string `json:"lang"`
	Code  string `json:"code"`
}

// RenderSnippets renders every snippet, failing on the first bad one.
func RenderSnippets(snippets []Snippet, ctx SnippetCtx) ([]RenderedSnippet, error) {
	out := make([]RenderedSnippet, 0, len(snippets))
	for _, s := range snippets {
		code, err := s.Render(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, RenderedSnippet{Title: s.Title, Lang: s.Lang, Code: code})
	}
	return out, nil
}
