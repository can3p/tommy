package as2

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	coreui "github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// UIBase is where the tab is mounted once the core has taken its prefix off.
const UIBase = UIPrefix

// ListLimit caps how many messages one render of the tab pulls out of the store.
const ListLimit = 500

// The AS2 tab exists rather than the generic event view because the question
// somebody has when an AS2 exchange goes wrong is never "what JSON did this
// produce". It is one of four:
//
//   - what came out of the wrapping, and is it the EDI I expected;
//   - what did the signature actually prove;
//   - what MIC did you compute, over what, and why does it not match theirs;
//   - what certificate do I have to import.
//
// Each of those has a panel. Everything on the page except tommy's own
// certificate came off the wire and is interpolated as a plain string by
// html/template; nothing here is ever template.HTML, and the payload is shown
// as text in a <pre> rather than being handed to the browser as content.

// uiHandler serves the AS2 tab.
type uiHandler struct {
	plugin *Plugin
	deps   plugin.Deps
	tpl    *template.Template
	tplErr error
}

func newUIHandler(p *Plugin, d plugin.Deps) *uiHandler {
	h := &uiHandler{plugin: p, deps: d}
	h.tpl, h.tplErr = coreui.PluginTemplates(p.Templates(), "*.html")
	return h
}

func (h *uiHandler) mount(mux plugin.Mux) {
	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /list", h.list)
	mux.HandleFunc("GET /messages/{id}", h.detail)
	mux.HandleFunc("DELETE /events", h.clear)
	// GET /events/{id} is deliberately left to the core's generic view, so an
	// event id from the API or the overview still opens a raw inspector here.
}

// tabView is everything the tab template renders.
type tabView struct {
	Base        string
	UIBase      string
	APIBase     string
	StreamEvent string

	Search   string
	Messages []listRow
	Selected *detailView
	Total    int

	Identity IdentityInfo

	// Info feeds the shared how-to-test panel, with snippets already rendered
	// against the ports this instance actually bound.
	Info []plugin.PluginInfo
}

// HowToTest carries the panel that says how to get messages in here, open when
// nothing has arrived yet - which is exactly when someone needs it.
func (v tabView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: !v.HasMessages()}
}

// HasMessages reports whether anything has been captured at all.
func (v tabView) HasMessages() bool { return v.Total > 0 }

// ListURL is where the message list fragment is refetched from.
func (v tabView) ListURL() string {
	if v.Search == "" {
		return v.Base + "/list"
	}
	return v.Base + "/list?search=" + url.QueryEscape(v.Search)
}

// CertificateURL is where a partner downloads the certificate to import.
func (v tabView) CertificateURL() string { return v.APIBase + "/certificate" }

// listRow is one message in the left-hand list. Every string on it is captured
// text and is interpolated by the template as a plain string.
type listRow struct {
	ID       string
	URL      string
	At       time.Time
	Route    string
	Subject  string
	Preview  string
	Selected bool
	Badges   []components.Badge
}

// detailView is the right-hand pane.
type detailView struct {
	ID       string
	Title    string
	At       time.Time
	Provider string
	Badges   []components.Badge

	RawURL     string
	PayloadURL string
	MDNURL     string
	EventURL   string

	Headers components.KVTable
	Layers  []layerRow

	// Assurance is the one sentence about what the signature proved. It is a
	// sentence rather than a tick because a tick would claim more than a
	// self-attested certificate can support.
	Assurance   string
	SignatureOK bool
	Signature   *Signature
	Encryption  *Encryption
	Compression *CompressionInfo

	MIC        *MIC
	Alternates []MIC

	Payload        Payload
	PayloadPreview string
	PayloadIsText  bool

	MDN *MDNRecord

	Issues []Issue
	Raw    event.Raw
}

// layerRow is one row of the onion table.
type layerRow struct {
	Depth  int
	Kind   string
	Label  string
	Detail string
	Opened bool
	Bytes  int
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, v)
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "as2-list", v)
}

func (h *uiHandler) detail(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v.Selected == nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if !coreui.IsPartial(r) {
		// A deep link renders the whole tab with the message open, so a
		// message URL can be pasted into a bug report.
		h.renderPage(w, r, v)
		return
	}
	h.writeFragment(w, "as2-detail", v)
}

func (h *uiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Store.Clear(r.Context(), Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, err := h.view(r, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "as2-tab", v)
}

func (h *uiHandler) view(r *http.Request, selectedID string) (tabView, error) {
	search := strings.TrimSpace(r.URL.Query().Get("search"))

	events, err := h.deps.Store.List(r.Context(), store.Query{
		Plugin: Name,
		Search: search,
		Limit:  ListLimit,
	})
	if err != nil {
		return tabView{}, err
	}

	v := tabView{
		Base:        UIBase,
		UIBase:      coreui.ShellFrom(r).UIBase,
		APIBase:     APIBase,
		StreamEvent: EventType,
		Search:      search,
		Info:        coreui.ShellFrom(r).Info(),
		Identity:    h.plugin.identity.Info(),
	}

	captured := Messages(events)
	v.Total = len(captured)
	if selectedID == "" && len(captured) > 0 {
		selectedID = string(captured[0].Event.ID)
	}
	for _, c := range captured {
		row := listRowFor(c, string(c.Event.ID) == selectedID)
		v.Messages = append(v.Messages, row)
		if row.Selected {
			d := h.detailFor(r, c)
			v.Selected = &d
		}
	}
	return v, nil
}

func listRowFor(c Captured, selected bool) listRow {
	m := c.Message
	return listRow{
		ID:       string(c.Event.ID),
		URL:      UIBase + "/messages/" + string(c.Event.ID),
		At:       c.Event.ReceivedAt,
		Route:    fallback(m.Route(), "(no AS2 identifiers)"),
		Subject:  m.Subject,
		Preview:  m.Preview(),
		Selected: selected,
		Badges:   messageBadges(m, c.Event.Provider),
	}
}

func (h *uiHandler) detailFor(r *http.Request, c Captured) detailView {
	m := c.Message
	id := string(c.Event.ID)
	d := detailView{
		ID:       id,
		Title:    m.Title(),
		At:       c.Event.ReceivedAt,
		Provider: c.Event.Provider,
		Badges:   messageBadges(m, c.Event.Provider),
		RawURL:   APIBase + "/messages/" + id + "/raw",
		EventURL: UIBase + "/events/" + id,
		Headers:  headerTable(m),
		Layers:   layerRows(m, h.deps.Now()),

		Signature:   m.Security.Signature,
		Encryption:  m.Security.Encryption,
		Compression: m.Security.Compression,
		MIC:         m.MIC,
		Alternates:  m.AlternateMICs,
		Payload:     m.Payload,
		MDN:         m.MDN,
		Issues:      m.Issues,
		Raw:         c.Event.Raw,
	}
	d.Assurance = m.Security.Signature.Assurance()
	d.SignatureOK = m.Security.Signature != nil && m.Security.Signature.Verified
	if m.Payload.Blob != nil {
		d.PayloadURL = APIBase + "/messages/" + id + "/payload"
	}
	if m.MDN != nil && m.MDN.Blob != nil {
		d.MDNURL = APIBase + "/messages/" + id + "/mdn"
	}
	d.PayloadIsText = m.Payload.Text()
	d.PayloadPreview = h.payloadExcerpt(r, m)
	return d
}

// PayloadExcerptLimit is how much of the document the tab inlines. The rest is
// a download away; a 20 MB interchange rendered into the page would make the
// tab unusable and tell nobody anything the first two kilobytes did not.
const PayloadExcerptLimit = 4096

// payloadExcerpt reads the start of the stored document for the detail pane.
//
// It is served into the page as an escaped plain string inside a <pre>, never
// as markup and never through an iframe. An EDI interchange is not HTML and has
// no business being treated as any, and captured content never enters the DOM
// as markup anywhere in tommy.
func (h *uiHandler) payloadExcerpt(r *http.Request, m *Message) string {
	if m.Payload.Blob == nil || !m.Payload.Text() || h.deps.Blobs == nil {
		return ""
	}
	rc, _, err := h.deps.Blobs.Open(r.Context(), m.Payload.Blob.ID)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, PayloadExcerptLimit)
	n, _ := rc.Read(buf)
	if n <= 0 {
		return ""
	}
	out := string(buf[:n])
	if m.Payload.Size > int64(n) {
		out += "\n… " + components.BytesHuman(m.Payload.Size-int64(n)) + " more; download the payload for the rest."
	}
	return out
}

// headerTable summarizes the AS2 headers. Every value is captured text and
// reaches the page as a plain string.
func headerTable(m *Message) components.KVTable {
	t := components.KVTable{Caption: "AS2 headers"}
	add := func(k, v string) {
		if v != "" {
			t.Rows = append(t.Rows, components.KV{Key: k, Value: v})
		}
	}
	add("AS2-From", m.From)
	add("AS2-To", m.To)
	add("Message-ID", m.MessageID)
	add("Subject", m.Subject)
	add("Date", m.Date)
	add("AS2-Version", m.AS2Version)
	add("Content-Type", m.ContentType)
	add("User-Agent", m.UserAgent)
	if m.Receipt.Requested {
		receipt := "requested"
		if m.Receipt.SignedRequested {
			receipt = "signed receipt requested (" + fallback(m.Receipt.SignedImportance, "unspecified") + ")"
		}
		if len(m.Receipt.MICAlgs) > 0 {
			receipt += ", micalg " + strings.Join(m.Receipt.MICAlgs, "/")
		}
		add("Receipt", receipt)
		add("Disposition-Notification-To", m.Receipt.NotifyTo)
	}
	if m.Receipt.AsyncURL != "" {
		add("Receipt-Delivery-Option", m.Receipt.AsyncURL+"  (asynchronous MDNs are not delivered; a synchronous one was returned instead)")
	}
	return t
}

// layerRows flattens the S/MIME onion into the table the template renders.
func layerRows(m *Message, now time.Time) []layerRow {
	rows := make([]layerRow, 0, len(m.Layers))
	for i, l := range m.Layers {
		row := layerRow{Depth: i, Kind: l.Kind, Bytes: l.Bytes, Opened: l.Opened}
		switch l.Kind {
		case LayerEncrypted:
			row.Label = "Encrypted"
			if enc := m.Security.Encryption; enc != nil {
				row.Detail = fallback(enc.ContentAlgorithm, enc.ContentAlgorithmOID)
				if enc.Error != "" {
					row.Detail = enc.Error
				}
			}
		case LayerSigned:
			row.Label = "Signed"
			if sig := m.Security.Signature; sig != nil {
				row.Detail = fallback(sig.DigestAlgorithm, sig.DeclaredMICAlg)
				if sig.Signer != nil {
					row.Detail += " · " + sig.Signer.Subject
					// An expired signing certificate is exactly the kind of
					// thing somebody runs tommy to discover, so it is said out
					// loud rather than enforced.
					if sig.Signer.Expired(now) {
						row.Detail += " · certificate expired " + sig.Signer.NotAfter.Format("2006-01-02")
					}
				}
			}
		case LayerCompressed:
			row.Label = "Compressed"
			if c := m.Security.Compression; c != nil {
				row.Detail = fallback(c.Algorithm, c.AlgorithmOID) + " · " + placementLabel(c.Placement)
				if r := c.Ratio(); r > 0 {
					row.Detail += fmt.Sprintf(" · %.0f%% smaller", r*100)
				}
				if c.Error != "" {
					row.Detail = c.Error
				}
			}
		default:
			row.Label = "Payload"
			row.Detail = fallback(l.ContentType, m.Payload.Format)
		}
		rows = append(rows, row)
	}
	return rows
}

func placementLabel(p string) string {
	switch p {
	case PlacementInner:
		return "compressed, then signed"
	case PlacementOuter:
		return "signed, then compressed"
	default:
		return "no signature"
	}
}

// messageBadges is the badge row on a list entry and above the detail.
//
// The signature badge is the one that has to be careful. "signature valid" is
// true and insufficient; "verified sender" would be false. So a valid signature
// with no partner certificate reads "signed · signer unverified", in the muted
// tone rather than the success one, and the full sentence is a hover away.
func messageBadges(m *Message, provider string) []components.Badge {
	var badges []components.Badge

	if sig := m.Security.Signature; sig != nil {
		switch {
		case !sig.Verified:
			badges = append(badges, components.Badge{Label: "signature invalid", Tone: "error", Title: sig.Assurance()})
		case sig.SignerMatched:
			badges = append(badges, components.Badge{Label: "signed by partner", Tone: "success", Title: sig.Assurance()})
		default:
			badges = append(badges, components.Badge{Label: "signed · signer unverified", Tone: "muted", Title: sig.Assurance()})
		}
	}
	if enc := m.Security.Encryption; enc != nil {
		if enc.Decrypted {
			badges = append(badges, components.Badge{
				Label: "encrypted " + fallback(enc.ContentAlgorithm, "(unknown algorithm)"),
				Tone:  "info",
				Title: "Decrypted with tommy's key",
			})
		} else {
			badges = append(badges, components.Badge{Label: "not decrypted", Tone: "error", Title: enc.Error})
		}
	}
	if c := m.Security.Compression; c != nil {
		tone, title := "info", placementLabel(c.Placement)
		if !c.Decompressed {
			tone, title = "error", c.Error
		}
		badges = append(badges, components.Badge{Label: "compressed", Tone: tone, Title: title})
	}
	if m.Payload.Format != "" {
		badges = append(badges, components.Badge{
			Label: m.Payload.Format,
			Tone:  "muted",
			Title: components.BytesHuman(m.Payload.Size),
		})
	}
	if n := countErrors(m.Issues); n > 0 {
		badges = append(badges, components.Badge{
			Label: strconv.Itoa(n) + " error" + plural(n),
			Tone:  "error",
			Title: issueSummary(m.Issues),
		})
	}
	if m.MDN != nil {
		label := "MDN"
		if m.MDN.Signed {
			label = "signed MDN"
		}
		badges = append(badges, components.Badge{Label: label, Tone: "muted", Title: m.MDN.Disposition})
	}
	if provider != "" {
		badges = append(badges, components.Badge{
			Label: provider,
			Tone:  "muted",
			Title: "Captured by the " + provider + " provider",
		})
	}
	return badges
}

func countErrors(issues []Issue) int {
	n := 0
	for _, i := range issues {
		if i.Severity == SeverityError {
			n++
		}
	}
	return n
}

func issueSummary(issues []Issue) string {
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		parts = append(parts, i.Detail)
	}
	return strings.Join(parts, "; ")
}

func (h *uiHandler) renderPage(w http.ResponseWriter, r *http.Request, v tabView) {
	body, err := h.render("as2-tab", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "AS2", body); err != nil {
		h.deps.Logger.Warn("render as2 tab", "err", err)
	}
}

func (h *uiHandler) render(name string, data any) (template.HTML, error) {
	if h.tplErr != nil {
		return "", h.tplErr
	}
	var buf bytes.Buffer
	if err := h.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // the template escapes every captured value; nothing here is raw markup
}

func (h *uiHandler) writeFragment(w http.ResponseWriter, name string, data any) {
	body, err := h.render(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
