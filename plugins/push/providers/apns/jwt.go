package apns

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// tokenAge is how long APNs accepts a provider token for. Apple's
// "Establishing a token-based connection to APNs": "If the value in the iat
// field is more than one hour old, APNs rejects any notifications containing
// the token, returning an ExpiredProviderToken (403) error."
//
// This provider never rejects on it. It records the fact instead, because a
// stale token is one of the two mistakes (the other being a wrong key id)
// that this capture exists to make visible.
const tokenAge = time.Hour

// jwtInfo is everything read out of the provider authentication token, and
// nothing that would require verifying it.
//
// The signature is never checked and no key is ever loaded. That is not a
// shortcut: tommy has no APNs signing key and could not verify one, and a
// fake that rejects credentials is useless (CLAUDE.md rule 1). What is worth
// having is the claims, because "wrong key id" and "token generated an hour
// ago and never refreshed" are real-world failures whose only symptom
// against Apple is a 403 with no detail.
type jwtInfo struct {
	// Alg and Kid come from the JWT header; Iss and Iat from the payload.
	// Apple documents exactly these four key/value pairs and no others.
	Alg string `json:"alg,omitempty"`
	Kid string `json:"kid,omitempty"`
	Iss string `json:"iss,omitempty"`
	// Iat is recorded exactly as it appeared, because Apple's own worked
	// example encodes it as a JSON *string* ("iat": "1459143580650"), not a
	// number - and that value is in milliseconds, not the seconds the field
	// is documented as. A parser that assumes a JSON number, or seconds,
	// gets nothing out of a token a real client generated.
	Iat any `json:"iat,omitempty"`
	// IssuedAt is Iat resolved to a time, when it could be. Empty otherwise.
	IssuedAt string `json:"issued_at,omitempty"`
	// Stale reports an iat more than an hour old - what Apple answers
	// ExpiredProviderToken to. Recorded, never acted on.
	Stale bool `json:"stale,omitempty"`
	// Header and Claims are the two decoded JWT segments in full, so a claim
	// this struct does not name is still captured. Apple's own example
	// token, for instance, has a header carrying only "kid" and no "alg" at
	// all.
	Header map[string]any `json:"header,omitempty"`
	Claims map[string]any `json:"claims,omitempty"`
	// Error says why the token could not be read. A malformed token is
	// recorded and served, never rejected: seeing what was actually sent is
	// the entire point.
	Error string `json:"error,omitempty"`
	// Verified is always false and is written out anyway, so nobody reading
	// a capture can mistake these claims for authenticated facts.
	Verified bool `json:"verified"`
}

// parseAuthorization reads an "authorization: bearer <jwt>" header.
//
// Apple spells the scheme lowercase ("bearer <provider_token>", and
// sideshow/apns2 sends exactly that), while RFC 6750 spells it "Bearer".
// Both are accepted, along with a bare token and no scheme at all, because
// every one of those is something a real client has been seen to send and
// none of them is a reason to lose a capture.
func parseAuthorization(header string, now time.Time) *jwtInfo {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	raw := header
	if scheme, rest, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "bearer") {
		raw = strings.TrimSpace(rest)
	}
	return parseJWT(raw, now)
}

// parseJWT decodes the two readable segments of a JWS compact serialization.
// The third segment is the signature and is never touched.
func parseJWT(raw string, now time.Time) *jwtInfo {
	info := &jwtInfo{}
	if raw == "" {
		info.Error = "authorization header carries no token"
		return info
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		info.Error = fmt.Sprintf("not a JWS compact serialization: %d dot-separated segments, want 3", len(parts))
		return info
	}

	var err error
	if info.Header, err = decodeSegment(parts[0]); err != nil {
		info.Error = "header segment: " + err.Error()
	}
	if info.Claims, err = decodeSegment(parts[1]); err != nil {
		if info.Error != "" {
			info.Error += "; "
		}
		info.Error += "claims segment: " + err.Error()
	}

	info.Alg = stringClaim(info.Header, "alg")
	info.Kid = stringClaim(info.Header, "kid")
	info.Iss = stringClaim(info.Claims, "iss")
	if info.Claims != nil {
		if v, ok := info.Claims["iat"]; ok {
			info.Iat = v
			if t, ok := issuedAt(v); ok {
				info.IssuedAt = t.UTC().Format(time.RFC3339)
				info.Stale = now.Sub(t) > tokenAge
			}
		}
	}
	return info
}

// decodeSegment base64url-decodes one JWT segment into a generic object. JWT
// segments are unpadded (RFC 7515 §2), but a padded one is decoded too rather
// than reported as malformed.
func decodeSegment(seg string) (map[string]any, error) {
	enc := base64.RawURLEncoding
	if strings.HasSuffix(seg, "=") {
		enc = base64.URLEncoding
	}
	data, err := enc.DecodeString(seg)
	if err != nil {
		return nil, fmt.Errorf("not base64url: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return obj, nil
}

func stringClaim(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	switch v := obj[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// issuedAt resolves an iat claim that may be a JSON number or a string, and
// that may be in seconds or - as in Apple's own documented example - in
// milliseconds. Anything past the year 5138 in seconds is read as
// milliseconds; that is the only heuristic available and it is stated here
// rather than hidden.
func issuedAt(v any) (time.Time, bool) {
	var n int64
	switch t := v.(type) {
	case float64:
		n = int64(t)
	case json.Number:
		parsed, err := t.Int64()
		if err != nil {
			return time.Time{}, false
		}
		n = parsed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		n = parsed
	default:
		return time.Time{}, false
	}
	if n <= 0 {
		return time.Time{}, false
	}
	if n > 1e11 {
		return time.UnixMilli(n), true
	}
	return time.Unix(n, 0), true
}
