package resend

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// stringList decodes Resend's `string | string[]` union, which `to`, `cc`,
// `bcc` and `reply_to` all use. Both spellings really do occur in practice and
// in the same request: resend-go marshals to/cc/bcc as arrays but reply_to as
// a bare string, so accepting only one form breaks the official Go SDK.
type stringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (l *stringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*l = nil
		return nil
	}
	if trimmed[0] == '"' {
		var one string
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*l = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return err
	}
	*l = stringList(many)
	return nil
}

// attachmentContent decodes an attachment's `content`, which has three
// spellings in the wild and no single documented one:
//
//   - a base64 string, which is what the REST reference documents and what
//     curl and the Node SDK send;
//   - a JSON array of byte values, which is what resend-go sends - its
//     Attachment.MarshalJSON runs the bytes through BytesToIntArray "in the
//     way Resend supports";
//   - {"type":"Buffer","data":[...]}, which is what a Node Buffer serializes
//     to if it reaches JSON.stringify unconverted.
//
// All three are accepted. Anything else is an invalid attachment.
type attachmentContent struct {
	data    []byte
	present bool
}

// errBadContent is returned for a `content` that is present but undecodable.
var errBadContent = errors.New("attachment content is neither base64 nor a byte array")

// UnmarshalJSON implements json.Unmarshaler.
func (c *attachmentContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		c.data, c.present = nil, false
		return nil
	}

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		decoded, err := decodeBase64(s)
		if err != nil {
			return err
		}
		c.data, c.present = decoded, true
		return nil

	case '[':
		var nums []int
		if err := json.Unmarshal(trimmed, &nums); err != nil {
			return errBadContent
		}
		out := make([]byte, len(nums))
		for i, n := range nums {
			if n < 0 || n > 255 {
				return fmt.Errorf("attachment content byte %d is out of range: %d", i, n)
			}
			out[i] = byte(n)
		}
		c.data, c.present = out, true
		return nil

	case '{':
		var buf struct {
			Type string `json:"type"`
			Data []int  `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &buf); err != nil || buf.Type != "Buffer" {
			return errBadContent
		}
		out := make([]byte, len(buf.Data))
		for i, n := range buf.Data {
			if n < 0 || n > 255 {
				return fmt.Errorf("attachment content byte %d is out of range: %d", i, n)
			}
			out[i] = byte(n)
		}
		c.data, c.present = out, true
		return nil
	}
	return errBadContent
}

// decodeBase64 accepts both the padded and the unpadded spelling, since a
// hand-rolled client is as likely to produce one as the other.
func decodeBase64(s string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	decoded, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

// wireTag is one entry of tags[]: {"name":"category","value":"confirm_email"}.
type wireTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// wireTemplate is the `template` object: a published template's id or alias,
// plus its variables. tommy records it and renders nothing - see the README.
type wireTemplate struct {
	ID        string         `json:"id"`
	Variables map[string]any `json:"variables,omitempty"`
}

// wireAttachment is one entry of attachments[]. `inline_content_id` is
// resend-go's deprecated spelling of content_id, kept because that SDK still
// marshals it.
type wireAttachment struct {
	Content         attachmentContent `json:"content"`
	Filename        string            `json:"filename"`
	Path            string            `json:"path"`
	ContentType     string            `json:"content_type"`
	ContentID       string            `json:"content_id"`
	InlineContentID string            `json:"inline_content_id"`
}

// cid returns whichever content id spelling the sender used.
func (a wireAttachment) cid() string {
	if a.ContentID != "" {
		return a.ContentID
	}
	return a.InlineContentID
}

// emailRequest is one email, the body of POST /emails and one element of the
// array POST /emails/batch takes. `react` is deliberately absent: it is a
// Node-SDK-only field that the SDK renders to `html` before the request
// leaves the process, so it never reaches the wire.
//
// html and text are pointers so "absent" and "explicitly empty" stay
// distinguishable - the reference makes that distinction load-bearing, since
// setting text to an empty string is the documented way to opt out of Resend
// generating a plain-text part from the HTML.
type emailRequest struct {
	From        string            `json:"from"`
	To          stringList        `json:"to"`
	Cc          stringList        `json:"cc"`
	Bcc         stringList        `json:"bcc"`
	ReplyTo     stringList        `json:"reply_to"`
	Subject     string            `json:"subject"`
	HTML        *string           `json:"html"`
	Text        *string           `json:"text"`
	Headers     map[string]string `json:"headers"`
	Attachments []wireAttachment  `json:"attachments"`
	Tags        []wireTag         `json:"tags"`
	ScheduledAt string            `json:"scheduled_at"`
	TopicID     string            `json:"topic_id"`
	Template    *wireTemplate     `json:"template"`
}

// sendResponse is the success body of POST /emails: {"id": "<uuid>"}.
type sendResponse struct {
	ID string `json:"id"`
}

// batchResponse is the success body of POST /emails/batch. The `errors` field
// the official SDKs decode is deliberately never populated; see the README.
type batchResponse struct {
	Data []sendResponse `json:"data"`
}

// emailResource is the body of GET /emails/{id}, field for field as the live
// reference's example shows it - `object` first, nullable text/html/scheduled_at,
// and cc/bcc/reply_to always arrays even though send accepts a bare string.
type emailResource struct {
	Object      string    `json:"object"`
	ID          string    `json:"id"`
	MessageID   string    `json:"message_id"`
	To          []string  `json:"to"`
	From        string    `json:"from"`
	CreatedAt   string    `json:"created_at"`
	Subject     string    `json:"subject"`
	HTML        *string   `json:"html"`
	Text        *string   `json:"text"`
	Bcc         []string  `json:"bcc"`
	Cc          []string  `json:"cc"`
	ReplyTo     []string  `json:"reply_to"`
	LastEvent   string    `json:"last_event"`
	ScheduledAt *string   `json:"scheduled_at"`
	Tags        []wireTag `json:"tags"`
}
