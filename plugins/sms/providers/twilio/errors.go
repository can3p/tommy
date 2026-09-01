package twilio

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Twilio's own error codes, reused verbatim so a client asserting on the
// number gets the answer the real API would give it.
const (
	codeAuth        = 20003 // Permission Denied / invalid credentials
	codeNotFound    = 20404 // Not Found
	codeInvalidTo   = 21211 // The 'To' number is not a valid phone number
	codeMissingBody = 21602 // The Body or MediaUrl parameter is required
	codeMissingFrom = 21603 // A From or MessagingServiceSid parameter is required
	codeMissingTo   = 21604 // The destination 'To' phone number is required
)

// errorBody is Twilio's REST error envelope, the same shape for every 4xx
// response the API returns.
type errorBody struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
	Status   int    `json:"status"`
}

func newErrorBody(status, code int, message string) errorBody {
	return errorBody{
		Code:     code,
		Message:  message,
		MoreInfo: fmt.Sprintf("https://www.twilio.com/docs/errors/%d", code),
		Status:   status,
	}
}

// writeError writes Twilio's error shape.
func writeError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, newErrorBody(status, code, message))
}

// writeJSON writes v as the JSON body, matching Twilio's own content type and
// leaving '+' and other URL characters unescaped the way its API does.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
