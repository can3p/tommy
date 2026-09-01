package mailjet

import "encoding/json"

// The types in this file are the wire shapes of Mailjet's Send API v3.1, as
// verified against https://dev.mailjet.com/email/guides/send-api-v31/ and its
// "Send API errors" / "Sandbox Mode" / "Send with attached files" subpages.
// Field names and casing must match Mailjet's exactly: this is what real SDKs
// (mailjet-apiv3-go and friends) serialize.

// wireAddress is Mailjet's `{"Email":"...","Name":"..."}` address shape, used
// for From, each entry of To/Cc/Bcc, and the single ReplyTo object.
type wireAddress struct {
	Email string `json:"Email"`
	Name  string `json:"Name,omitempty"`
}

// wireAttachment is one entry of "Attachments": a regular, downloadable part.
type wireAttachment struct {
	ContentType   string `json:"ContentType"`
	Filename      string `json:"Filename"`
	Base64Content string `json:"Base64Content"`
}

// wireInlineAttachment is one entry of "InlinedAttachments": the same shape as
// wireAttachment plus the ContentID an HTML body references as `cid:ID`.
type wireInlineAttachment struct {
	ContentType   string `json:"ContentType"`
	Filename      string `json:"Filename"`
	ContentID     string `json:"ContentID"`
	Base64Content string `json:"Base64Content"`
}

// wireMessage is one entry of the request's top-level "Messages" array. Each
// entry is one *delivered* message, so it becomes exactly one mail.Message and
// one event, matching mail.Message's own doc comment.
type wireMessage struct {
	From               wireAddress            `json:"From"`
	To                 []wireAddress          `json:"To,omitempty"`
	Cc                 []wireAddress          `json:"Cc,omitempty"`
	Bcc                []wireAddress          `json:"Bcc,omitempty"`
	ReplyTo            *wireAddress           `json:"ReplyTo,omitempty"` // a single object, not a list - unlike To/Cc/Bcc
	Subject            string                 `json:"Subject,omitempty"`
	TextPart           string                 `json:"TextPart,omitempty"`
	HTMLPart           string                 `json:"HTMLPart,omitempty"`
	Headers            map[string]string      `json:"Headers,omitempty"`
	Attachments        []wireAttachment       `json:"Attachments,omitempty"`
	InlinedAttachments []wireInlineAttachment `json:"InlinedAttachments,omitempty"`
	CustomID           string                 `json:"CustomID,omitempty"`
	// EventPayload is left as raw JSON rather than a fixed Go type: Mailjet
	// documents it as an arbitrary value echoed back on webhook events, and
	// nothing in the guide pins it to a string versus an object.
	EventPayload   json.RawMessage `json:"EventPayload,omitempty"`
	CustomCampaign string          `json:"CustomCampaign,omitempty"`
}

// sendRequest is the whole POST /v3.1/send body.
type sendRequest struct {
	Messages    []wireMessage `json:"Messages"`
	SandboxMode bool          `json:"SandboxMode,omitempty"`
}

// recipientResult is one entry of a success result's To/Cc/Bcc, matching
// https://dev.mailjet.com/docs/email-api/send-api-v31/sandbox.mode's example
// response byte for byte (field names and nesting).
type recipientResult struct {
	Email       string `json:"Email"`
	MessageUUID string `json:"MessageUUID"`
	MessageID   int64  `json:"MessageID"`
	MessageHref string `json:"MessageHref"`
}

// successResult is one Messages[] entry of a successful response.
type successResult struct {
	Status   string            `json:"Status"`
	CustomID string            `json:"CustomID"`
	To       []recipientResult `json:"To"`
	Cc       []recipientResult `json:"Cc"`
	Bcc      []recipientResult `json:"Bcc"`
}

// detailError is one entry of a per-message error's "Errors" array. Mailjet
// documents that "for a single message, Send API can return multiple errors,
// each related to different properties of the payload" - tommy's fake always
// returns exactly one, since it stops at the first problem it finds.
type detailError struct {
	ErrorIdentifier string   `json:"ErrorIdentifier"`
	ErrorCode       string   `json:"ErrorCode"`
	StatusCode      int      `json:"StatusCode"`
	ErrorMessage    string   `json:"ErrorMessage"`
	ErrorRelatedTo  []string `json:"ErrorRelatedTo,omitempty"`
}

// errorResult is one Messages[] entry when that particular message failed
// validation. Per Mailjet's "Send API errors" page, a per-message failure
// still rides inside a 200 response: only a malformed request as a whole
// (global error, below) is a top-level 4xx.
type errorResult struct {
	Errors []detailError `json:"Errors"`
	Status string        `json:"Status"`
}

// sendResponse is the whole response body: Messages[] mixes successResult and
// errorResult entries positionally, one per request entry.
type sendResponse struct {
	Messages []any `json:"Messages"`
}

// globalError is returned instead of sendResponse when the request as a whole
// could not be processed at all (bad JSON, empty Messages[], bad auth),
// matching https://dev.mailjet.com/docs/email-api/send-api-v31/send-api-errors
// verbatim: {"ErrorIdentifier","ErrorCode","StatusCode","ErrorMessage"}.
type globalError struct {
	ErrorIdentifier string   `json:"ErrorIdentifier"`
	ErrorCode       string   `json:"ErrorCode"`
	StatusCode      int      `json:"StatusCode"`
	ErrorMessage    string   `json:"ErrorMessage"`
	ErrorRelatedTo  []string `json:"ErrorRelatedTo,omitempty"`
}
