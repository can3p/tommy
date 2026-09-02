package fcm

import (
	"encoding/json"
	"net/http"
)

// apiError is the Google API JSON error envelope every non-2xx FCM v1
// response carries: {"error": {"code","message","status","details"}}. FCM
// does not invent its own error body the way SendGrid or Mailjet do - it uses
// the same shape as every other Google API built on the same infrastructure.
// Verified against Firebase's published error-codes reference
// (https://firebase.google.com/docs/cloud-messaging/error-codes) for the
// details envelope and against the standard Google API UNAUTHENTICATED body
// (documented across several Google API products - see the package README
// for what was checked) for the credentials-rejected case.
type apiError struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Status  string   `json:"status"`
	Details []detail `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

// detail is one entry of error.details. A malformed request uses
// google.rpc.BadRequest (FieldViolations), which is the only kind this
// provider ever emits - see wireError's doc comment for why the other
// documented kind, google.firebase.fcm.v1.FcmError (ErrorCode: "UNREGISTERED"
// etc.), is deliberately never produced here.
type detail struct {
	Type            string           `json:"@type"`
	FieldViolations []fieldViolation `json:"fieldViolations,omitempty"`
	ErrorCode       string           `json:"errorCode,omitempty"`
}

type fieldViolation struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

// badRequestType is the @type of a google.rpc.BadRequest detail.
const badRequestType = "type.googleapis.com/google.rpc.BadRequest"

// wireError is a resolved (status, body) pair for one FCM-shaped error
// response.
//
// This provider only ever returns INVALID_ARGUMENT (malformed request body,
// unresolvable or ambiguous targeting) and UNAUTHENTICATED (a pinned bearer
// token that was not presented, or presented wrong). It deliberately never
// returns UNREGISTERED, SENDER_ID_MISMATCH, QUOTA_EXCEEDED or the other
// delivery-time codes documented at
// https://firebase.google.com/docs/cloud-messaging/error-codes: those
// describe what happens when FCM actually tries to reach a device - whether
// a token is still registered, whether the sender owns it, whether the
// project is over quota - and tommy has no registry of tokens, no sender
// identity and no delivery pipeline to be honest about. Inventing "this
// specific token is unregistered" would be tommy deciding which requests
// fail, which is the policy-making this project's charter (CLAUDE.md, "What
// tommy is") rules out; it captures and answers, it does not simulate
// scenarios.
type wireError struct {
	httpStatus int
	body       errorEnvelope
}

func (e *wireError) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.httpStatus)
	_ = json.NewEncoder(w).Encode(e.body)
}

func newError(httpStatus int, status, message string, details ...detail) *wireError {
	return &wireError{httpStatus: httpStatus, body: errorEnvelope{Error: apiError{
		Code: httpStatus, Message: message, Status: status, Details: details,
	}}}
}

func errBadJSON(err error) *wireError {
	return newError(http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid JSON payload received. "+err.Error())
}

func errTooLarge() *wireError {
	return newError(http.StatusBadRequest, "INVALID_ARGUMENT", "the request body exceeds the maximum allowed size")
}

func errMessageRequired() *wireError {
	return newError(http.StatusBadRequest, "INVALID_ARGUMENT", "message: Required. Message to send.",
		detail{Type: badRequestType, FieldViolations: []fieldViolation{
			{Field: "message", Description: "Required. Message to send."},
		}})
}

func errInvalidMessage(err error) *wireError {
	return newError(http.StatusBadRequest, "INVALID_ARGUMENT", "message: "+err.Error(),
		detail{Type: badRequestType, FieldViolations: []fieldViolation{
			{Field: "message", Description: err.Error()},
		}})
}

func errNoTarget() *wireError {
	return newError(http.StatusBadRequest, "INVALID_ARGUMENT",
		"message: exactly one of token, fid, topic or condition must be specified",
		detail{Type: badRequestType, FieldViolations: []fieldViolation{
			{Field: "message", Description: "exactly one of token, fid, topic or condition is required"},
		}})
}

func errMultipleTargets() *wireError {
	return newError(http.StatusBadRequest, "INVALID_ARGUMENT",
		"message: only one of token, fid, topic or condition may be specified",
		detail{Type: badRequestType, FieldViolations: []fieldViolation{
			{Field: "message", Description: "at most one of token, fid, topic or condition may be set"},
		}})
}

// errUnauthenticated is the standard Google API 401 body, documented
// identically across several Google API products (Cloud Speech, Google Ads,
// Apps Script, ...) - Google APIs built on the same infrastructure share one
// authentication-failure body rather than each inventing their own.
func errUnauthenticated() *wireError {
	return newError(http.StatusUnauthorized, "UNAUTHENTICATED",
		"Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other "+
			"valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project.")
}

func errInternal(err error) *wireError {
	return newError(http.StatusInternalServerError, "INTERNAL", err.Error())
}
