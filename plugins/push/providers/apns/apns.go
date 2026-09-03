// Package apns imitates the Apple Push Notification service provider API -
// POST /3/device/{deviceToken} - well enough that sideshow/apns2, or any
// HTTP/2 client pointed at tommy's ingress, sends a push and gets back the
// response it expects.
//
// It mounts ordinary routes on the shared ingress. It builds no listener and
// no HTTP/2 server of its own: the ingress serves cleartext HTTP/2 alongside
// HTTP/1.1 on one port (net/http's Server.Protocols with
// SetUnencryptedHTTP2), prior-knowledge only, and it is on by default. See
// "When h2c is off" in README.md for the one thing that changes when it is
// turned off, and handle's downgrade note for what this provider does about
// it.
//
// # What was checked against live vendor documentation
//
// Apple's documentation pages are rendered by JavaScript, so fetching the
// HTML yields a title and nothing else. The tables were read from the JSON
// the pages are built from, fetched directly:
//
//   - .../usernotifications/sending-notification-requests-to-apns.json -
//     the request header table and the eleven apns-push-type values.
//   - .../usernotifications/handling-notification-responses-from-apns.json -
//     the response header table, the status codes, and the full reason list.
//   - .../usernotifications/generating-a-remote-notification.json - Tables
//     1, 2 and 3 (the aps, alert and sound dictionaries) and the payload
//     size limits.
//   - .../usernotifications/establishing-a-token-based-connection-to-apns.json -
//     the four JWT key/value pairs and the one-hour iat rule.
//
// under https://developer.apple.com/tutorials/data/documentation/. What each
// one contradicted is recorded at the code it affects and summarized in
// README.md.
package apns

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/push"
)

// ProviderName is this provider's name: the URL segment it is listed under
// and the value stamped on every event it produces.
const ProviderName = "apns"

// DevicePathPattern is the real provider API path, unchanged.
const DevicePathPattern = "/3/device/{deviceToken}"

// DeviceCatchAllPattern catches everything else under /3/device/ so that the
// two path mistakes a client can actually make - no device token at all, and
// extra path segments - get APNs' own error shapes (MissingDeviceToken and
// BadPath) instead of tommy's generic "no provider handles this" page. Both
// are decided by the request alone, so neither is a scenario being
// simulated.
const DeviceCatchAllPattern = "/3/device/{rest...}"

// Provider is the Apple Push Notification service fake.
type Provider struct{}

// New returns the APNs provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return push.Name }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "Imitates Apple's push provider API, POST /3/device/{deviceToken}, over the ingress's cleartext HTTP/2. " +
		"It records every apns-* header and the unverified claims of the ES256 provider token - a wrong key id or a " +
		"token generated an hour ago is exactly what you came to see - and answers with the real empty-bodied 200 " +
		"plus apns-id, or Apple's own {\"reason\":\"...\"} error for a request that is malformed on its face."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method: "POST",
			Path:   DevicePathPattern,
			Description: "Accept an APNs notification payload for the device token in the path, record it as a " +
				"push.message event, and answer 200 with an empty body and an apns-id header.",
		},
		{
			Method: "POST",
			Path:   DeviceCatchAllPattern,
			Description: "Answer APNs' own MissingDeviceToken or BadPath error for a request under /3/device/ " +
				"whose path carries no device token, or carries extra segments after it.",
		},
	}
}

// Snippets implements plugin.Provider.
//
// Both use curl --http2-prior-knowledge, and they have to: APNs is an
// HTTP/2-only API, tommy serves it without TLS, and the Upgrade: h2c
// handshake was removed from HTTP/2 by RFC 9113 and is answered here as
// HTTP/1.1. Prior knowledge is the one handshake that reaches an h2c
// listener, and it is what every HTTP/2-only vendor client performs.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Send an APNs alert over cleartext HTTP/2",
			Lang:  "bash",
			Code: `curl -s -i --http2-prior-knowledge -X POST \
  {{.IngressURL}}/3/device/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0 \
  -H 'apns-topic: com.example.MyApp' \
  -H 'apns-push-type: alert' \
  -H 'apns-priority: 10' \
  -H 'authorization: bearer eyJhbGciOiJFUzI1NiIsImtpZCI6IjhZTDNHM1JSWDcifQ.eyJpc3MiOiJDODZOVjlKWDNEIiwiaWF0IjoxNzU2ODAwMDAwfQ.c2lnbmF0dXJlLW5ldmVyLXZlcmlmaWVk' \
  -d '{"aps":{"alert":{"title":"Game Request","subtitle":"Five Card Draw","body":"Bob wants to play poker"},"badge":1,"sound":"default","category":"GAME_INVITATION"},"gameID":"12345678"}'`,
		},
		{
			Title: "Send a silent background push",
			Lang:  "bash",
			Code: `curl -s -i --http2-prior-knowledge -X POST \
  {{.IngressURL}}/3/device/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0 \
  -H 'apns-topic: com.example.MyApp' \
  -H 'apns-push-type: background' \
  -H 'apns-priority: 5' \
  -d '{"aps":{"content-available":1}}'`,
		},
	}
}

// RegisterIngress implements plugin.Provider.
//
// Both routes are mounted without a method so this provider can answer
// MethodNotAllowed itself. net/http's ServeMux would otherwise reject a GET
// with a bare 405 and an empty body, and a client that expects Apple's
// {"reason":"MethodNotAllowed"} would see nothing it can decode.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc(DevicePathPattern, func(w http.ResponseWriter, r *http.Request) {
		handle(w, r, d)
	})
	mux.HandleFunc(DeviceCatchAllPattern, func(w http.ResponseWriter, r *http.Request) {
		handleBadPath(w, r)
	})
}

func handleBadPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		newError(http.StatusMethodNotAllowed, reasonMethodNotAllowed).write(w, "")
		return
	}
	if strings.TrimSpace(r.PathValue("rest")) == "" {
		newError(http.StatusBadRequest, reasonMissingDeviceToken).write(w, "")
		return
	}
	newError(http.StatusNotFound, reasonBadPath).write(w, "")
}

// handle serves POST /3/device/{deviceToken}.
func handle(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	if r.Method != http.MethodPost {
		// "The request used an invalid :method value. Only POST requests are
		// supported."
		newError(http.StatusMethodNotAllowed, reasonMethodNotAllowed).write(w, "")
		return
	}

	// A request that reached here over HTTP/1.1 cannot have come from a real
	// APNs client - Apple has been HTTP/2 only since the binary protocol was
	// dropped in 2021. It is still served and still captured, because losing
	// a capture over a transport detail helps nobody, but it is called out
	// three ways: a header the caller sees, a line in the log, and a key on
	// the event. The usual cause is `[ingress] h2c = false` (or --h2c=false)
	// and a client that fell back, or a client that asked for the deprecated
	// Upgrade: h2c handshake, which RFC 9113 removed and net/http answers
	// over HTTP/1.1.
	downgraded := r.ProtoMajor < 2
	if downgraded {
		w.Header().Set("tommy-warning", "this request arrived over "+r.Proto+
			"; APNs is HTTP/2 only. Use prior-knowledge h2c (curl --http2-prior-knowledge) and check [ingress] h2c is not disabled.")
		d.Logger.Warn("apns request arrived over HTTP/1.1; APNs is HTTP/2 only",
			"proto", r.Proto, "path", r.URL.Path,
			"hint", "use prior-knowledge h2c; check [ingress] h2c / --h2c")
	}

	h, werr := readHeaders(r.Header, newUUID)
	if werr != nil {
		werr.write(w, h.ID)
		return
	}

	presented := r.Header.Get(headerAuthorization)
	claims := parseAuthorization(presented, d.Now())

	// Credentials are accepted by default and only ever checked against a
	// value the config pins (CLAUDE.md rule 1). Nothing here verifies a
	// signature - tommy has no signing key and could not.
	if werr := checkPins(d, h, claims); werr != nil {
		werr.write(w, h.ID)
		return
	}

	limit := h.maxBodyFor()
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		newError(http.StatusBadRequest, reasonPayloadEmpty).write(w, h.ID)
		return
	}
	if len(body) > limit {
		newError(http.StatusRequestEntityTooLarge, reasonPayloadTooLarge).write(w, h.ID)
		return
	}
	if len(body) == 0 {
		newError(http.StatusBadRequest, reasonPayloadEmpty).write(w, h.ID)
		return
	}

	conv := buildMessage(r.PathValue("deviceToken"), h, body)

	ev := push.NewEvent(ProviderName, conv.Message)
	ev.Raw = event.Raw{
		Transport: push.Transport,
		PeerAddr:  r.RemoteAddr,
		Method:    r.Method,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		Body:      body,
		Text:      true,
	}
	if ev.Meta == nil {
		ev.Meta = map[string]any{}
	}
	ev.Meta["apns_id"] = h.ID
	ev.Meta["apns_id_supplied"] = h.IDSupplied
	if len(h.All) > 0 {
		// Every apns-* header, verbatim and whole, including one this
		// provider does not model. Event.Raw.Headers has them too, but Meta
		// is what the push API filters and the event list shows.
		ev.Meta["apns_headers"] = h.All
	}
	if presented != "" {
		ev.Meta["authorization"] = presented
	}
	if claims != nil {
		ev.Meta["jwt"] = claims
	}
	ev.Meta["http_version"] = r.Proto
	if downgraded {
		ev.Meta["warning"] = "arrived over " + r.Proto + "; APNs is HTTP/2 only"
	}
	if conv.PayloadError != "" {
		ev.Meta["payload_error"] = conv.PayloadError
	}
	for k, v := range conv.Aps {
		ev.Meta["aps_"+k] = v
	}

	if err := d.Append(r.Context(), ev); err != nil {
		newError(http.StatusInternalServerError, reasonInternalServerError).write(w, h.ID)
		return
	}

	writeSuccess(w, h.ID, string(ev.ID))
}

// checkPins is the only path on which this provider rejects a credential or a
// topic, and it is reached only when the config asked for it.
func checkPins(d plugin.Deps, h headers, claims *jwtInfo) *wireError {
	if topic := strings.TrimSpace(d.Config.String("topic", "")); topic != "" && h.Topic != topic {
		// "Pushing to this topic is not allowed." - the topic is well formed,
		// it is just not one this configuration accepts. That is what
		// TopicDisallowed says; BadTopic is for a malformed value.
		return newError(http.StatusBadRequest, reasonTopicDisallowed)
	}
	if keyID := strings.TrimSpace(d.Config.String("key_id", "")); keyID != "" {
		presented := ""
		if claims != nil {
			presented = claims.Kid
		}
		if presented != keyID {
			// "The provider token is not valid, or the token signature can't
			// be verified." The signature half is never why this fires here;
			// the key id is.
			return newError(http.StatusForbidden, reasonInvalidProviderToken)
		}
	}
	return nil
}

// writeSuccess answers exactly what Apple's sample success response shows:
//
//	apns-id = eabeae54-14a8-11e5-b60b-1697f925ec7b
//	:status = 200
//
// and nothing else - no body, and no content-type, since there is no content.
//
// apns-unique-id is added because Apple documents it as returned "only ...
// in the Development environment", and a local catcher reachable over
// cleartext is nothing else. Apple specifies no format for it, so it carries
// tommy's own event id: that makes the response correlate with
// GET /api/v1/push/messages/{id}, the same trick the fcm provider plays with
// its message name.
func writeSuccess(w http.ResponseWriter, apnsID, eventID string) {
	w.Header().Set(headerAPNsID, apnsID)
	w.Header().Set(headerAPNsUniqueID, eventID)
	w.WriteHeader(http.StatusOK)
}

// newUUID mints the apns-id APNs would have created for a request that sent
// none: "32 lowercase hexadecimal digits, displayed in five groups separated
// by hyphens in the form 8-4-4-4-12".
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns an error on any supported platform;
		// answering with a well-formed all-zero UUID beats failing a capture.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
