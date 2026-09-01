package twilio

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/sms"
)

// maxBody caps how much of a request this provider will read, the same limit
// the sms fake and fakeplugin providers use.
const maxBody = 1 << 20

// create handles POST .../Messages.json: Twilio's send call. The body is
// application/x-www-form-urlencoded, never JSON, and MediaUrl may repeat.
func (p *Provider) create(d plugin.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountSid := r.PathValue("AccountSid")

		user, pass, hasAuth := r.BasicAuth()
		if !authOK(d, user, pass) {
			writeError(w, http.StatusUnauthorized, codeAuth, "Authentication Error - invalid username")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, http.StatusBadRequest, "could not read request body")
			return
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			writeError(w, http.StatusBadRequest, http.StatusBadRequest, "malformed form-encoded body")
			return
		}

		to := strings.TrimSpace(values.Get("To"))
		from := strings.TrimSpace(values.Get("From"))
		messagingServiceSid := strings.TrimSpace(values.Get("MessagingServiceSid"))
		textBody := values.Get("Body")
		mediaURLs := values["MediaUrl"]
		statusCallback := strings.TrimSpace(values.Get("StatusCallback"))

		switch {
		case to == "":
			writeError(w, http.StatusBadRequest, codeMissingTo, "The destination 'To' phone number is required to send an SMS")
			return
		case !sms.IsE164(to):
			writeError(w, http.StatusBadRequest, codeInvalidTo, fmt.Sprintf("The 'To' number %s is not a valid phone number.", to))
			return
		case from == "" && messagingServiceSid == "":
			writeError(w, http.StatusBadRequest, codeMissingFrom, "A 'From' phone number or a 'MessagingServiceSid' is required to send a message")
			return
		case textBody == "" && len(mediaURLs) == 0:
			writeError(w, http.StatusBadRequest, codeMissingBody, "The 'Body' or 'MediaUrl' parameter is required to send a Message with Twilio")
			return
		}

		msg := &sms.Message{
			From:             from,
			To:               to,
			MessagingService: messagingServiceSid,
			Body:             textBody,
			Media:            mediaFromURLs(mediaURLs),
		}
		msg.Normalize()

		id := d.NewID()
		sid := sidFor(id, msg.IsMMS())
		base := accountBase(accountSid)
		m := meta{
			Sid:        sid,
			AccountSid: accountSid,
			APIVersion: apiVersion,
			URI:        base + "/Messages/" + sid + ".json",
			SubresourceURIs: map[string]string{
				"media":    base + "/Messages/" + sid + "/Media.json",
				"feedback": base + "/Messages/" + sid + "/Feedback.json",
			},
			StatusCallback: statusCallback,
			BasicAuth: basicAuthMeta{
				Presented:  hasAuth,
				AccountSid: user,
				AuthToken:  pass,
			},
		}

		ev := &event.Event{
			ID:       event.ID(id),
			Plugin:   sms.Name,
			Provider: Name,
			Type:     sms.EventType,
			Summary:  msg.EventSummary(),
			Meta:     m.toMap(),
			Payload:  msg,
			Raw: event.Raw{
				Transport: "http",
				PeerAddr:  r.RemoteAddr,
				Method:    r.Method,
				Path:      r.URL.Path,
				Headers:   r.Header.Clone(),
				Body:      body,
				Text:      true,
			},
		}
		if err := d.Append(r.Context(), ev); err != nil {
			writeError(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, buildResource(sid, accountSid, msg, ev.ReceivedAt))
	}
}

// authOK reports whether the presented Basic-auth credentials are acceptable.
// Twilio's sandbox accepts anything by default; only a provider config that
// pins account_sid/auth_token turns this into a real check, and then it fails
// closed on a mismatch.
func authOK(d plugin.Deps, user, pass string) bool {
	pinnedSid := d.Config.String("account_sid", "")
	pinnedToken := d.Config.String("auth_token", "")
	if pinnedSid == "" && pinnedToken == "" {
		return true
	}
	return user == pinnedSid && pass == pinnedToken
}

// list handles GET .../Messages.json, serving whatever this provider has
// recorded in the store rather than keeping a parallel copy of its own -
// an SDK that creates and then lists sees its own write.
func (p *Provider) list(d plugin.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountSid := r.PathValue("AccountSid")

		events, err := d.Store.List(r.Context(), store.Query{Plugin: sms.Name, Provider: Name})
		if err != nil {
			writeError(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
			return
		}

		items := make([]resource, 0, len(events))
		for _, c := range sms.Messages(events) {
			evAccountSid, sid := metaOf(c.Event.Meta)
			if accountSid != "" && evAccountSid != accountSid {
				continue
			}
			items = append(items, buildResource(sid, evAccountSid, c.Message, c.Event.ReceivedAt))
		}

		base := accountBase(accountSid) + "/Messages.json"
		writeJSON(w, http.StatusOK, listEnvelope{
			Messages:     items,
			Page:         0,
			PageSize:     len(items),
			Start:        0,
			End:          endOf(len(items)),
			URI:          base,
			FirstPageURI: base,
		})
	}
}

// listEnvelope is Twilio's paged list response shape.
type listEnvelope struct {
	Messages        []resource `json:"messages"`
	Page            int        `json:"page"`
	PageSize        int        `json:"page_size"`
	Start           int        `json:"start"`
	End             int        `json:"end"`
	URI             string     `json:"uri"`
	FirstPageURI    string     `json:"first_page_uri"`
	NextPageURI     *string    `json:"next_page_uri"`
	PreviousPageURI *string    `json:"previous_page_uri"`
}

func endOf(n int) int {
	if n == 0 {
		return 0
	}
	return n - 1
}

// fetch handles GET .../Messages/{Sid}.json, again straight from the store.
func (p *Provider) fetch(d plugin.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountSid := r.PathValue("AccountSid")
		sid := r.PathValue("Sid")

		notFound := func() {
			writeError(w, http.StatusNotFound, codeNotFound,
				fmt.Sprintf("The requested resource %s was not found", r.URL.Path))
		}

		id, ok := idFromSid(sid)
		if !ok {
			notFound()
			return
		}
		e, err := d.Store.Get(r.Context(), event.ID(id))
		if err != nil {
			notFound()
			return
		}
		if e.Plugin != sms.Name || e.Provider != Name || e.Type != sms.EventType {
			notFound()
			return
		}
		m, ok := sms.MessageOf(e)
		if !ok {
			notFound()
			return
		}
		evAccountSid, storedSid := metaOf(e.Meta)
		if storedSid == "" || (accountSid != "" && evAccountSid != accountSid) {
			notFound()
			return
		}

		writeJSON(w, http.StatusOK, buildResource(storedSid, evAccountSid, m, e.ReceivedAt))
	}
}
