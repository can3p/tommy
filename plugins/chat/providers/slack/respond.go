package slack

import (
	"encoding/json"
	"io"
	"net/http"
)

// writeWebhookOK writes Slack's incoming-webhook success response: literally
// the text "ok", not JSON. This is the single easiest thing to get wrong about
// this surface.
func writeWebhookOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// writeWebhookError writes one of Slack's incoming-webhook errors: plain text,
// the literal error code as the body, with the real HTTP status Slack uses for
// that class of failure (400 for a malformed request, 404 for an unknown
// webhook).
func writeWebhookError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, code)
}

// writeAPI writes a chat.postMessage-shaped JSON response. Slack's Web API
// returns HTTP 200 for both success and application-level failure alike -
// verified against the live method reference, which never documents a
// different status for "ok":false - so every caller of this passes 200; only
// a request that never made it to the handler (a 405 from the mux on the
// wrong verb, say) would ever see anything else.
func writeAPI(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeAPIError writes chat.postMessage's error envelope: {"ok":false,"error":"<code>"}.
func writeAPIError(w http.ResponseWriter, code string) {
	writeAPI(w, map[string]any{"ok": false, "error": code})
}
