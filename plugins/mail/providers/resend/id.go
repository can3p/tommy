package resend

import "strings"

// Resend addresses an email by a UUID ("49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"),
// while tommy's event ids are 24 lowercase hex characters. GET /emails/{id}
// therefore needs to get from one to the other, and it does it the way the
// twilio provider gets from a Sid back to an event id: by a reversible
// encoding, not by a second index. Nothing here is stored anywhere.
//
// A tommy event id is 12 bytes; a UUID is 16, of which 6 bits are spoken for
// by the version and variant fields. Laying the 24 hex characters of the event
// id into the 30 free hex positions of a v4 UUID leaves 6 spare, which carry a
// fixed marker so an id this provider never minted is recognized as such
// instead of being decoded into nonsense:
//
//	eeeeeeee-eeee-4eee-aeee-eeeeeeMMMMMM
//	          ^event id             ^marker
//
// The version nibble is 4 and the variant nibble is a, so every id is a
// syntactically valid v4 UUID for a client that parses one (the .NET SDK
// parses these into a Guid), and every id tommy mints ends in the marker.
const idMarker = "facade"

// emailIDFor maps an event id to the id the API hands back.
//
// The fallback matters: a caller may inject a NewID that does not produce 24
// hex characters, and a fake must still answer with something that round-trips
// rather than silently minting an id nothing can fetch. Such an id is passed
// through unchanged - it is not UUID-shaped, but it is reversible, which is
// the property GET depends on.
func emailIDFor(eventID string) string {
	if !isHex24(eventID) {
		return eventID
	}
	e := eventID
	return e[0:8] + "-" + e[8:12] + "-4" + e[12:15] + "-a" + e[15:18] + "-" + e[18:24] + idMarker
}

// eventIDFromEmailID reverses emailIDFor, reporting false for anything this
// provider did not mint.
func eventIDFromEmailID(emailID string) (string, bool) {
	s := strings.ToLower(strings.ReplaceAll(emailID, "-", ""))
	if len(s) != 32 || !isHexString(s) {
		return "", false
	}
	if s[12] != '4' || s[16] != 'a' || s[26:] != idMarker {
		return "", false
	}
	return s[0:12] + s[13:16] + s[17:20] + s[20:26], true
}

// looksLikeUUID reports whether s has the 8-4-4-4-12 hex shape, which is what
// separates "an email id that is simply not here" (404) from "that is not an
// email id at all" (422), the two answers the real API gives.
func looksLikeUUID(s string) bool {
	s = strings.ToLower(s)
	if len(s) != 36 {
		return false
	}
	for _, i := range []int{8, 13, 18, 23} {
		if s[i] != '-' {
			return false
		}
	}
	return isHexString(strings.ReplaceAll(s, "-", ""))
}

func isHex24(s string) bool { return len(s) == 24 && isHexString(s) }

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return len(s) > 0
}
