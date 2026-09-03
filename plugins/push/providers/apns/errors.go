package apns

import (
	"encoding/json"
	"net/http"
)

// Reason strings APNs returns in the "reason" key of an error body.
//
// Verified against Apple's "Handling notification responses from APNs",
// fetched live from
// https://developer.apple.com/tutorials/data/documentation/usernotifications/handling-notification-responses-from-apns.json
// (the JSON behind the JavaScript-rendered documentation page, which is what
// makes the table machine-readable at all). Only the reasons this provider
// can honestly produce are declared here; see errors.go's package-level
// discussion below and README.md for the ones deliberately left out.
const (
	// 400 - determinable from the request alone.
	reasonBadCollapseID      = "BadCollapseId"
	reasonBadExpirationDate  = "BadExpirationDate"
	reasonBadMessageID       = "BadMessageId"
	reasonBadPriority        = "BadPriority"
	reasonDuplicateHeaders   = "DuplicateHeaders"
	reasonInvalidPushType    = "InvalidPushType"
	reasonMissingDeviceToken = "MissingDeviceToken"
	reasonMissingTopic       = "MissingTopic"
	reasonPayloadEmpty       = "PayloadEmpty"
	reasonTopicDisallowed    = "TopicDisallowed"

	// 403 - only ever produced when the config pins a key id.
	reasonInvalidProviderToken = "InvalidProviderToken"

	// 404 / 405 / 413 / 500.
	reasonBadPath             = "BadPath"
	reasonMethodNotAllowed    = "MethodNotAllowed"
	reasonPayloadTooLarge     = "PayloadTooLarge"
	reasonInternalServerError = "InternalServerError"
)

// wireError is a resolved (status, reason) pair for one APNs error response.
//
// The body is documented as a JSON dictionary with a "reason" key, plus a
// "timestamp" key that "is included only when the error in the :status field
// is 410". This provider never answers 410 - see README.md - so no error it
// builds carries a timestamp, and the field is deliberately absent from this
// struct rather than present and never set.
type wireError struct {
	status int
	reason string
}

func newError(status int, reason string) *wireError {
	return &wireError{status: status, reason: reason}
}

// write sends the error exactly as Apple's sample response shows it:
//
//	:status = 400
//	content-type = application/json
//	apns-id: <a_UUID>
//	{ "reason" : "BadDeviceToken" }
//
// apns-id is echoed on errors too, because the request header table says APNs
// "includes this value when reporting the error to your server" - a client
// correlating a failure with the notification it sent needs it there.
func (e *wireError) write(w http.ResponseWriter, apnsID string) {
	if apnsID != "" {
		w.Header().Set(headerAPNsID, apnsID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(struct {
		Reason string `json:"reason"`
	}{Reason: e.reason})
}
