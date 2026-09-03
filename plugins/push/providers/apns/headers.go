package apns

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/can3p/tommy/plugins/push"
)

// The request headers APNs defines for POST /3/device/{deviceToken}, from the
// header-field table in Apple's "Sending notification requests to APNs".
// There are no others: :method, :path and authorization are the rest of that
// table, and the first two are the request line.
const (
	headerAPNsID         = "apns-id"
	headerAPNsExpiration = "apns-expiration"
	headerAPNsPriority   = "apns-priority"
	headerAPNsTopic      = "apns-topic"
	headerAPNsPushType   = "apns-push-type"
	headerAPNsCollapseID = "apns-collapse-id"
	headerAuthorization  = "authorization"

	// headerAPNsUniqueID is a *response* header: "An identifier that is only
	// available in the Development environment. Use this to query Delivery
	// Log information for the corresponding notification in Push
	// Notifications Console."
	headerAPNsUniqueID = "apns-unique-id"
)

// pushTypes is every value Apple documents for apns-push-type, in the order
// the "Know when to use push types" section presents them. There are eleven,
// which is why push.Message.PushType is a free string and this list is only
// used to answer InvalidPushType - a twelfth value Apple adds later is a
// wire-format change, not a tommy release.
//
// Worth noting against a plausible-looking alternative source:
// sideshow/apns2, the Go client this provider is tested with, knows only
// nine of them. It has no constant for "controls" or "widgets". Taking the
// list from the client rather than from Apple would have produced a fake
// that rejects two push types the real service accepts.
var pushTypes = []string{
	"alert", "background", "complication", "controls", "fileprovider",
	"liveactivity", "location", "mdm", "pushtotalk", "voip", "widgets",
}

func validPushType(v string) bool {
	for _, t := range pushTypes {
		if v == t {
			return true
		}
	}
	return false
}

// priorities are the three values Apple documents for apns-priority: 10
// (immediately), 5 (power considerations) and 1 (prioritize the device's
// power considerations over everything else). push.PriorityOf maps exactly
// these three onto the plugin's normalized levels.
var priorities = map[string]bool{"1": true, "5": true, "10": true}

// canonicalUUID matches the apns-id format Apple specifies: "32 lowercase
// hexadecimal digits, displayed in five groups separated by hyphens in the
// form 8-4-4-4-12".
//
// It is matched case-insensitively even though the documentation says
// lowercase. Rejecting an upper-case UUID would lose a capture over a
// cosmetic difference that clients (including Go's own
// google/uuid.New().String() callers who upper-case it) produce all the
// time, and "the apns-id value is bad" is meant for a value that is not a
// UUID at all.
var canonicalUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// maxCollapseID is the documented limit on apns-collapse-id: "The value of
// this key must not exceed 64 bytes."
const maxCollapseID = 64

// Payload size limits, from "Generating a remote notification": "For Voice
// over Internet Protocol (VoIP) notifications, the maximum payload size is 5
// KB (5120 bytes). For all other remote notifications, the maximum payload
// size is 4 KB (4096 bytes)."
const (
	maxPayload     = 4096
	maxVoIPPayload = 5120
)

// headers is the validated view of one request's APNs headers.
type headers struct {
	// ID is the apns-id to answer with: the client's when it sent one,
	// otherwise the one this provider minted.
	ID string
	// IDSupplied records which of those two it was, since "APNs creates a
	// UUID for you and returns it" is a meaningful difference to a client
	// trying to correlate a push with a log line.
	IDSupplied bool

	Topic      string
	PushType   string
	Priority   string
	Expiration string
	CollapseID string

	// All is every apns-* header the request carried, exactly as sent. It is
	// recorded wholesale rather than field by field so a header Apple adds
	// later still shows up in a capture without a code change here.
	All map[string]string
}

// apnsHeaderNames lists the headers checked for repetition. HTTP/2 allows a
// field to appear more than once and APNs answers DuplicateHeaders to it,
// which is a genuine client bug this provider can see without inventing any
// state.
var apnsHeaderNames = []string{
	headerAPNsID, headerAPNsExpiration, headerAPNsPriority, headerAPNsTopic,
	headerAPNsPushType, headerAPNsCollapseID, headerAuthorization,
}

// readHeaders validates the APNs request headers and reports the first
// problem as the error APNs itself would answer with.
//
// The order is: repeated headers, then apns-id (so the response can echo it
// even when something later fails), then the rest. Apple does not document a
// validation order, so this one is chosen to make the response as useful as
// possible rather than to claim fidelity it cannot verify.
func readHeaders(h http.Header, newID func() string) (headers, *wireError) {
	var out headers
	out.All = map[string]string{}
	for name, values := range h {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "apns-") && len(values) > 0 {
			out.All[lower] = values[0]
		}
	}

	for _, name := range apnsHeaderNames {
		if len(h.Values(name)) > 1 {
			return out, newError(http.StatusBadRequest, reasonDuplicateHeaders)
		}
	}

	out.ID = strings.TrimSpace(h.Get(headerAPNsID))
	if out.ID != "" {
		if !canonicalUUID.MatchString(out.ID) {
			return out, newError(http.StatusBadRequest, reasonBadMessageID)
		}
		out.IDSupplied = true
	} else {
		out.ID = newID()
	}

	out.Topic = strings.TrimSpace(h.Get(headerAPNsTopic))
	if out.Topic == "" {
		return out, newError(http.StatusBadRequest, reasonMissingTopic)
	}

	out.PushType = strings.TrimSpace(h.Get(headerAPNsPushType))
	if out.PushType != "" && !validPushType(out.PushType) {
		return out, newError(http.StatusBadRequest, reasonInvalidPushType)
	}

	out.Priority = strings.TrimSpace(h.Get(headerAPNsPriority))
	if out.Priority != "" && !priorities[out.Priority] {
		return out, newError(http.StatusBadRequest, reasonBadPriority)
	}

	out.Expiration = strings.TrimSpace(h.Get(headerAPNsExpiration))
	if out.Expiration != "" {
		if _, err := strconv.ParseInt(out.Expiration, 10, 64); err != nil {
			return out, newError(http.StatusBadRequest, reasonBadExpirationDate)
		}
	}

	out.CollapseID = h.Get(headerAPNsCollapseID)
	if len(out.CollapseID) > maxCollapseID {
		return out, newError(http.StatusBadRequest, reasonBadCollapseID)
	}

	return out, nil
}

// maxBodyFor is the payload limit that applies to this push. VoIP gets the
// larger one.
func (h headers) maxBodyFor() int {
	if h.PushType == "voip" {
		return maxVoIPPayload
	}
	return maxPayload
}

// applyTo copies the delivery-shaped headers onto the canonical message.
// apns-topic deliberately does not appear here: it is the app's bundle ID and
// belongs in Message.App, never in Target - see push.Target's own warning.
func (h headers) applyTo(m *push.Message) {
	m.PushType = h.PushType
	m.App = h.Topic
	m.Delivery.SetPriority(h.Priority)
	m.Delivery.CollapseKey = h.CollapseID
	if h.Expiration != "" {
		// ParseInt already succeeded in readHeaders. push.ExpiresAt owns the
		// zero sentinel: apns-expiration 0 means "try once, do not store",
		// which is not 1 January 1970.
		n, _ := strconv.ParseInt(h.Expiration, 10, 64)
		m.Delivery.Expiry = push.ExpiresAt(n, h.Expiration)
	}
}
