package resend

import (
	"encoding/json"
	"net/http"
)

// Resend's error names, from the live error reference
// (https://resend.com/docs/api-reference/errors). Only the ones this fake can
// legitimately produce are listed; the rest describe quota, suppression and
// delivery state tommy does not keep.
const (
	codeInvalidIdempotencyKey = "invalid_idempotency_key" // 400
	codeValidationError       = "validation_error"        // 400
	codeMissingAPIKey         = "missing_api_key"         // 401
	codeNotFound              = "not_found"               // 404
	codeInvalidAttachment     = "invalid_attachment"      // 422
	codeInvalidParameter      = "invalid_parameter"       // 422
	codeMissingRequiredField  = "missing_required_field"  // 422
	codeApplicationError      = "application_error"       // 500
)

// Messages the live documentation, or an official SDK's own fixtures, spell
// out verbatim. Anything composed rather than quoted is marked at its use.
const (
	msgMissingAPIKey    = "Missing API key in the authorization header."
	msgValidationError  = "An error was found with one or more fields in the request."
	msgIdempotencyKey   = "Idempotency keys, if present, must have between 1 and 256 characters."
	msgInvalidAttach    = "Attachment must have either a `content` or `path`."
	msgEmailNotFound    = "Email not found"
	msgApplicationError = "An unexpected error occurred."
)

// errorBody is the JSON every Resend error response carries. The shape is
// {name, message, statusCode} - documented nowhere in the reference itself,
// but written down verbatim in the official Node SDK's own response fixtures
// (resend/resend-node, src/emails/emails.spec.ts) and in its ErrorResponse
// interface, which is where the statusCode field was confirmed
// (resend/resend-node#286).
type errorBody struct {
	Name       string `json:"name"`
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode"`
}

func newError(status int, name, message string) errorBody {
	return errorBody{Name: name, Message: message, StatusCode: status}
}

// writeError renders a Resend error response. HTML escaping is off for the
// same reason it is off in writeJSON: the real API's error messages contain
// "Name <email@example.com>" verbatim, not "Name \u003cemail...".
func writeError(w http.ResponseWriter, body errorBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(body.StatusCode)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// missingField is Resend's 422 for a required field left out. The message
// wording - "Missing `from` field." - is quoted from the official Node SDK's
// response fixture for exactly this case.
func missingField(field string) errorBody {
	return newError(http.StatusUnprocessableEntity, codeMissingRequiredField, "Missing `"+field+"` field.")
}

// invalidAddress is Resend's 422 for an address that is not RFC 5322 shaped.
// The wording is quoted from a real API response reported in
// resend/resend-node#286; note that the SDK's own hand-written fixture for
// the `from` case omits the trailing full stop, so one of the two is slightly
// off - this reproduces the one that came off the wire.
func invalidAddress(field string) errorBody {
	return newError(http.StatusUnprocessableEntity, codeInvalidParameter,
		"Invalid `"+field+"` field. The email address needs to follow the `email@example.com` or `Name <email@example.com>` format.")
}
