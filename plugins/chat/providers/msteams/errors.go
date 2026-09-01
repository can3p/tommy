package msteams

import "net/http"

// writeError answers with the plain-text error body a real Teams webhook
// endpoint gives back - unlike SendGrid or Twilio, there is no documented
// JSON error envelope here; the real "bad payload" error a caller sees back
// from Teams reads as a plain string ("Summary or Text is required.", per the
// live troubleshooting docs), not a JSON object.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message))
}
