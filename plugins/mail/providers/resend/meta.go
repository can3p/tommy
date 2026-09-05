package resend

import "encoding/json"

// meta is everything Resend-specific about one captured message. None of it
// belongs on the canonical mail.Message - that struct is the part every mail
// provider agrees on - so it all lands on Event.Meta, and buildResource reads
// back out of here what GET /emails/{id} has to report.
type meta struct {
	// EmailID is the UUID the send call answered with, and the id a client
	// fetches by.
	EmailID string
	// MessageID is the RFC 5322 Message-ID the retrieve endpoint reports.
	MessageID string
	// IdempotencyKey is recorded and otherwise ignored: tommy never
	// deduplicates, because remembering which keys were seen is state.
	IdempotencyKey string
	// BatchIndex and BatchSize are set only for /emails/batch, and are what
	// tells the fanned-out events apart.
	BatchIndex *int
	BatchSize  *int
	// BatchValidation is the x-batch-validation header, recorded but never
	// acted on - see the README for why permissive mode is not implemented.
	BatchValidation string
	// Authorization is whatever credential was presented, accepted or not.
	Authorization string
	// Request is the parsed email, for the fields tommy records without
	// acting on them.
	Request *emailRequest
	// Remote are the attachments that named a URL instead of carrying bytes.
	// tommy never fetches them.
	Remote []remoteAttachment

	// Read-back fields, filled by metaOf.
	ScheduledAt string
	Tags        []wireTag
	HasHTML     bool
	HasText     bool
}

// Meta keys. They are the provider's own namespace on the event.
const (
	metaEmailID         = "email_id"
	metaMessageID       = "message_id"
	metaIdempotencyKey  = "idempotency_key"
	metaBatchIndex      = "batch_index"
	metaBatchSize       = "batch_size"
	metaBatchValidation = "batch_validation"
	metaAuthorization   = "authorization"
	metaScheduledAt     = "scheduled_at"
	metaTopicID         = "topic_id"
	metaTags            = "tags"
	metaTemplate        = "template"
	metaRemote          = "remote_attachments"
	metaHasHTML         = "has_html"
	metaHasText         = "has_text"
)

// toMap renders the metadata onto the event. Keys whose value the sender did
// not supply are left out entirely, so the UI's metadata panel shows what was
// sent rather than a wall of empty strings.
func (m meta) toMap() map[string]any {
	out := map[string]any{
		metaEmailID:   m.EmailID,
		metaMessageID: m.MessageID,
	}
	if m.IdempotencyKey != "" {
		out[metaIdempotencyKey] = m.IdempotencyKey
	}
	if m.BatchIndex != nil {
		out[metaBatchIndex] = *m.BatchIndex
	}
	if m.BatchSize != nil {
		out[metaBatchSize] = *m.BatchSize
	}
	if m.BatchValidation != "" {
		out[metaBatchValidation] = m.BatchValidation
	}
	if m.Authorization != "" {
		out[metaAuthorization] = m.Authorization
	}
	if len(m.Remote) > 0 {
		out[metaRemote] = m.Remote
	}
	if r := m.Request; r != nil {
		if r.ScheduledAt != "" {
			out[metaScheduledAt] = r.ScheduledAt
		}
		if r.TopicID != "" {
			out[metaTopicID] = r.TopicID
		}
		if len(r.Tags) > 0 {
			out[metaTags] = r.Tags
		}
		if r.Template != nil {
			out[metaTemplate] = r.Template
		}
		// html and text are pointers on the wire so that "absent" and
		// "explicitly empty" stay apart; the canonical Message flattens both
		// to "", so which one it was is recorded here and read back by
		// buildResource, whose response distinguishes null from "".
		out[metaHasHTML] = r.HTML != nil
		out[metaHasText] = r.Text != nil
	}
	return out
}

// metaOf reads back the parts of the metadata the retrieve endpoint needs.
// The in-memory store shares values, so the common path is a type assertion;
// the JSON fallback keeps this honest for a store that serializes.
func metaOf(raw map[string]any) meta {
	m := meta{}
	if raw == nil {
		return m
	}
	m.EmailID = stringValue(raw[metaEmailID])
	m.MessageID = stringValue(raw[metaMessageID])
	m.IdempotencyKey = stringValue(raw[metaIdempotencyKey])
	m.ScheduledAt = stringValue(raw[metaScheduledAt])
	m.Tags = tagsValue(raw[metaTags])
	m.HasHTML = boolValue(raw[metaHasHTML])
	m.HasText = boolValue(raw[metaHasText])
	return m
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func tagsValue(v any) []wireTag {
	switch t := v.(type) {
	case nil:
		return nil
	case []wireTag:
		return t
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var tags []wireTag
	if err := json.Unmarshal(encoded, &tags); err != nil {
		return nil
	}
	return tags
}
