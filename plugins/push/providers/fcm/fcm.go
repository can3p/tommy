// Package fcm imitates Firebase Cloud Messaging's HTTP v1 send API well
// enough that the Firebase Admin SDKs, or any HTTP client pointed at tommy's
// ingress, can send a push and get back the response shape they expect.
//
// It mounts one route, POST /v1/projects/{project}/messages:send, on the
// shared ingress - the API is plain HTTP/1.1-compatible JSON with an OAuth2
// bearer token, so unlike the apns provider that follows it, it needs nothing
// from the core beyond what already exists.
//
// # What was checked against live vendor documentation
//
// Every wire field name here was verified against the discovery document
// served at https://fcm.googleapis.com/$discovery/rest?version=v1, fetched
// directly and parsed as JSON - not summarized by an intermediary and not
// taken from the plan or from the push plugin core's own comments. The
// discovery document's schema.properties keys, which are themselves the
// canonical JSON field names, are lowerCamelCase throughout - collapseKey,
// restrictedPackageName, notificationCount, notificationPriority,
// clickAction, channelId, titleLocKey, bodyLocKey, validateOnly, fcmOptions.
//
// That is the canonical spelling, not the only one: FCM v1 is a proto3-backed
// API, and the proto3 JSON mapping spec
// (https://protobuf.dev/programming-guides/json/) states that "parsers accept
// both the lowerCamelCase name ... and the original proto field name" - so a
// real client may legitimately send either collapseKey or collapse_key, and
// both must be read the same way. That is not the deprecated Legacy HTTP
// API's spelling leaking in - the push plugin core's message.go doc
// comments, README.md and fake_test.go, which use the underscore form, are
// describing the same v1 endpoint's other valid spelling, not a different
// API. wire.go's normalizeKeys is what makes this provider honor both; see
// its doc comment and the package README for the fidelity bug an earlier
// version of this provider had before that existed (accepting snake_case
// input with a 200 and then silently dropping the field).
//
// The success and error response shapes were checked separately: the success
// body ({"name": "projects/{project}/messages/{message_id}"} with HTTP 200)
// against Firebase's own HTTP v1 guide and real-world examples of the id
// format; the error envelope against
// https://firebase.google.com/docs/cloud-messaging/error-codes, which is the
// standard Google API error shape ({"error": {"code","message","status","details"}})
// used across Google's APIs rather than something FCM invented.
package fcm

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/push"
)

// ProviderName is this provider's name: the URL segment it is listed under
// and the value stamped on every event it produces.
const ProviderName = "fcm"

// SendPathPattern is the route this provider mounts, matching the real API's
// flat path exactly (v1/projects/{projectsId}/messages:send in the discovery
// document).
const SendPathPattern = "/v1/projects/{project}/messages:send"

// MaxBody caps the request body. FCM documents no single published limit for
// a v1 send request the way SendGrid documents 30MB; 1MB is generous headroom
// over any realistic notification-plus-data-plus-overrides payload while
// still bounding what an anti-DoS reader has to hold in memory.
const MaxBody = 1 << 20

// Provider is the Firebase Cloud Messaging HTTP v1 fake.
type Provider struct{}

// New returns the FCM provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return push.Name }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "Imitates Firebase Cloud Messaging's HTTP v1 send API. It accepts any bearer token, records which of " +
		"the four addressing fields (token, fid, topic, condition) was actually used, lifts the android/apns/" +
		"webpush override blocks out as their own inspectable payloads, and answers with the real " +
		"{\"name\":\"projects/.../messages/...\"} success shape or Google's standard error envelope."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method: "POST",
		Path:   SendPathPattern,
		Description: "Accept a SendMessageRequest ({\"message\":{...},\"validateOnly\":bool}), record it as a " +
			"push.message event, and answer with the real {\"name\":\"projects/{project}/messages/{id}\"} success shape.",
	}}
}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Send an FCM notification to a topic",
			Lang:  "bash",
			Code: `curl -s -X POST {{.IngressURL}}/v1/projects/my-project/messages:send \
  -H 'Authorization: Bearer any-oauth-access-token' \
  -H 'Content-Type: application/json' \
  -d '{"message":{"topic":"weather","notification":{"title":"Storm warning","body":"Batten down the hatches"}}}'`,
		},
		{
			Title: "Send a data-only FCM message with an Android override",
			Lang:  "bash",
			Code: `curl -s -X POST {{.IngressURL}}/v1/projects/my-project/messages:send \
  -H 'Authorization: Bearer any-oauth-access-token' \
  -H 'Content-Type: application/json' \
  -d '{"message":{"token":"cQAdeviceRegistrationToken","data":{"kind":"refresh"},"android":{"priority":"HIGH","ttl":"3600s","collapseKey":"weather"}}}'`,
		},
	}
}

// RegisterIngress implements plugin.Provider.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST "+SendPathPattern, func(w http.ResponseWriter, r *http.Request) {
		handleSend(w, r, d)
	})
}

// checkAuth accepts any bearer token by default (CLAUDE.md rule 1) and only
// rejects when the provider config pins an expected value (config key
// "bearer_token"), using the standard Google API 401 shape on a mismatch.
func checkAuth(d plugin.Deps, presented string) *wireError {
	expected := d.Config.String("bearer_token", "")
	if expected == "" {
		return nil
	}
	if presented != "Bearer "+expected {
		return errUnauthenticated()
	}
	return nil
}

// buildFromRequest validates addressing and converts an already-decoded
// SendMessageRequest into the canonical push.Message, or reports exactly
// which FCM-shaped 400 to answer with.
func buildFromRequest(project string, env sendMessageRequest) (*push.Message, *wireError) {
	if emptyRaw(env.Message) {
		return nil, errMessageRequired()
	}
	// normalizeKeys is what lets msg's struct tags be camelCase-only and
	// still match a request that spelled its fields snake_case - see
	// wireMessage's and normalizeKeys' doc comments in wire.go. env.Message
	// itself is untouched by this: normalizeKeys returns a new, separate
	// []byte, so the bytes buildMessage later puts in the FormatFCM payload
	// are still exactly what was sent.
	var msg wireMessage
	if err := json.Unmarshal(normalizeKeys(env.Message), &msg); err != nil {
		return nil, errInvalidMessage(err)
	}

	targets := 0
	for _, v := range []string{msg.Token, msg.Fid, msg.Topic, msg.Condition} {
		if v != "" {
			targets++
		}
	}
	switch {
	case targets == 0:
		return nil, errNoTarget()
	case targets > 1:
		return nil, errMultipleTargets()
	}

	return buildMessage(project, env.Message, msg), nil
}

// handleSend serves POST /v1/projects/{project}/messages:send.
func handleSend(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	project := r.PathValue("project")

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		errBadJSON(err).write(w)
		return
	}
	if len(body) > MaxBody {
		errTooLarge().write(w)
		return
	}

	presented := r.Header.Get("Authorization")
	if werr := checkAuth(d, presented); werr != nil {
		werr.write(w)
		return
	}

	// "message" is in opaqueKeys, so normalizing the envelope here to catch
	// a validate_only spelling never touches the bytes env.Message ends up
	// holding - they still decode to exactly what the client sent.
	var env sendMessageRequest
	if err := json.Unmarshal(normalizeKeys(body), &env); err != nil {
		errBadJSON(err).write(w)
		return
	}

	m, werr := buildFromRequest(project, env)
	if werr != nil {
		werr.write(w)
		return
	}

	ev := push.NewEvent(ProviderName, m)
	ev.Raw = event.Raw{
		Transport: "http",
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
	// validateOnly means the caller asked FCM not to actually deliver this.
	// tommy never delivers anything regardless, so the honest thing to record
	// is not silence but the flag itself: a test suite that sets validateOnly
	// by mistake and cannot see why nothing shows up needs this in the
	// capture, not a capture that quietly vanished. This mirrors the mail
	// plugin's mailjet provider, which records SandboxMode sends the same
	// way rather than skipping them - see plugins/mail/providers/mailjet.
	ev.Meta["validate_only"] = env.ValidateOnly
	if presented != "" {
		ev.Meta["authorization"] = presented
	}

	if err := d.Append(r.Context(), ev); err != nil {
		errInternal(err).write(w)
		return
	}

	writeSuccess(w, project, string(ev.ID))
}

func writeSuccess(w http.ResponseWriter, project, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Real message ids observed in the wild are shaped "0:<opaque>" (e.g.
	// "0:1692853925636648%31bd1c9631bd1c96"); the "0:" prefix is reproduced
	// for a closer read, the rest is tommy's own event id, which is what
	// lets a client that fetches this message back through the push API
	// correlate the two.
	_, _ = w.Write([]byte(`{"name":"projects/` + project + `/messages/0:` + id + `"}`))
}
