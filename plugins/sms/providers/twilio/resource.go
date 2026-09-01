package twilio

import (
	"strconv"
	"time"

	"github.com/can3p/tommy/plugins/sms"
)

// dateLayout is Twilio's timestamp format across date_created, date_sent and
// date_updated: RFC 1123 with a numeric zone, e.g.
// "Thu, 30 Jul 2015 20:12:31 +0000". It is the same layout as RFC 2822, which
// is what Twilio's own docs call it.
const dateLayout = time.RFC1123Z

// resource is the wire shape of a Twilio Message resource: what the create
// call returns with a 201, and what list/fetch return for a message already
// recorded. Field names and null-vs-empty handling are verified against
// https://www.twilio.com/docs/messaging/api/message-resource.
//
// num_media and num_segments are quoted integers in the real API, so they are
// plain strings here rather than json.Number or int.
type resource struct {
	AccountSid          string            `json:"account_sid"`
	APIVersion          string            `json:"api_version"`
	Body                string            `json:"body"`
	DateCreated         string            `json:"date_created"`
	DateSent            string            `json:"date_sent"`
	DateUpdated         string            `json:"date_updated"`
	Direction           string            `json:"direction"`
	ErrorCode           *int              `json:"error_code"`
	ErrorMessage        *string           `json:"error_message"`
	From                *string           `json:"from"`
	MessagingServiceSid *string           `json:"messaging_service_sid"`
	NumMedia            string            `json:"num_media"`
	NumSegments         string            `json:"num_segments"`
	Price               *string           `json:"price"`
	PriceUnit           *string           `json:"price_unit"`
	Sid                 string            `json:"sid"`
	Status              string            `json:"status"`
	SubresourceURIs     map[string]string `json:"subresource_uris"`
	To                  string            `json:"to"`
	URI                 string            `json:"uri"`
}

// meta is the sms.Event.Meta shape this provider records on every message it
// creates. It carries everything Twilio-specific: the resource identifiers
// and the credentials that were presented, never anything that belongs in the
// canonical sms.Message.
type meta struct {
	Sid             string            `json:"sid"`
	AccountSid      string            `json:"account_sid"`
	APIVersion      string            `json:"api_version"`
	URI             string            `json:"uri"`
	SubresourceURIs map[string]string `json:"subresource_uris"`
	StatusCallback  string            `json:"status_callback,omitempty"`
	BasicAuth       basicAuthMeta     `json:"basic_auth"`
}

// basicAuthMeta records exactly what credentials a request presented, per the
// "accept anything by default, record what was presented" rule. Nothing here
// is ever used to decide anything unless the provider config pins credentials.
type basicAuthMeta struct {
	Presented  bool   `json:"presented"`
	AccountSid string `json:"account_sid,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`
}

// toMap turns meta into the map[string]any Event.Meta expects. A struct with
// json tags round-trips identically whether or not the store ever re-encodes
// it, which keeps list/fetch honest about what create actually stored.
func (m meta) toMap() map[string]any {
	return map[string]any{
		"sid":              m.Sid,
		"account_sid":      m.AccountSid,
		"api_version":      m.APIVersion,
		"uri":              m.URI,
		"subresource_uris": m.SubresourceURIs,
		"status_callback":  m.StatusCallback,
		"basic_auth": map[string]any{
			"presented":   m.BasicAuth.Presented,
			"account_sid": m.BasicAuth.AccountSid,
			"auth_token":  m.BasicAuth.AuthToken,
		},
	}
}

// metaOf reads the fields list/fetch need back out of an event's Meta. Every
// other Twilio-specific field (the resource URIs, the presented credentials)
// is either re-derived from sid+accountSid or is nobody's business but the
// audit trail's.
func metaOf(m map[string]any) (accountSid, sid string) {
	accountSid, _ = m["account_sid"].(string)
	sid, _ = m["sid"].(string)
	return
}

// stringOrNil returns nil for an empty string so the JSON encoder emits null,
// matching Twilio's own handling of an unset field.
func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// buildResource converts a captured message plus its Twilio metadata into the
// wire resource. m.Segments must already be current - callers get that for
// free by going through sms.MessageOf, which re-normalizes on every read.
func buildResource(sid, accountSid string, m *sms.Message, receivedAt time.Time) resource {
	date := receivedAt.UTC().Format(dateLayout)
	base := accountBase(accountSid)
	uri := base + "/Messages/" + sid + ".json"
	subURIs := map[string]string{
		"media":    base + "/Messages/" + sid + "/Media.json",
		"feedback": base + "/Messages/" + sid + "/Feedback.json",
	}

	return resource{
		AccountSid:          accountSid,
		APIVersion:          apiVersion,
		Body:                m.Body,
		DateCreated:         date,
		DateSent:            date,
		DateUpdated:         date,
		Direction:           "outbound-api",
		ErrorCode:           nil,
		ErrorMessage:        nil,
		From:                stringOrNil(m.From),
		MessagingServiceSid: stringOrNil(m.MessagingService),
		NumMedia:            strconv.Itoa(len(m.Media)),
		NumSegments:         strconv.Itoa(m.Segments.Count),
		Price:               nil,
		PriceUnit:           nil,
		Sid:                 sid,
		Status:              string(m.Status),
		SubresourceURIs:     subURIs,
		To:                  m.To,
		URI:                 uri,
	}
}

// mediaFromURLs converts repeated MediaUrl form values into sms.Media. Twilio's
// MediaUrl is a link, not bytes: tommy never fetches it, so Blob stays nil and
// only the URL travels.
func mediaFromURLs(urls []string) []sms.Media {
	if len(urls) == 0 {
		return nil
	}
	out := make([]sms.Media, 0, len(urls))
	for _, u := range urls {
		if u == "" {
			continue
		}
		out = append(out, sms.Media{URL: u})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
