package msteams

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/chat"
)

// handleWebhook accepts a POST to the webhookb2 route, converts it into one
// chat.Message and answers with whichever generation's success contract the
// payload shape calls for.
//
// Decision rule, spelled out because it is the one thing easy to get wrong:
// a body carrying a top-level "@type":"MessageCard" is an Office 365 / M365
// connector card, and gets that generation's literal "1" (text/plain, 200).
// Everything else - an Adaptive Card wrapped in the Bot-Framework-shaped
// {"type":"message","attachments":[...]} envelope, or the bare
// {"text":"..."} shape Microsoft's own "Send a request to the webhook"
// example posts straight to a workflow trigger - is a Power Automate
// workflow trigger, and gets a 202 Accepted with an empty body. This is a
// payload-shape rule rather than a path or header rule because both
// generations share the identical webhookb2 URL shape; a workflow trigger can
// even accept a MessageCard, but the reverse is never true, so keying off
// "@type" is the only signal that never misclassifies a genuine connector
// card.
func handleWebhook(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	ctx := r.Context()

	guidTenant := r.PathValue("GuidTenant")
	id := r.PathValue("ID")
	key := r.PathValue("Key")
	guid, tenant, _ := strings.Cut(guidTenant, "@")

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(body) > MaxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "Message size limit exceeded.")
		return
	}

	var sniff wireSniff
	if err := json.Unmarshal(body, &sniff); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid payload: request body is not valid JSON.")
		return
	}

	isConnectorCard := strings.EqualFold(sniff.AtType, "MessageCard")

	meta := map[string]any{
		"guid":   guid,
		"tenant": tenant,
		"id":     id,
		"key":    key,
		"path":   r.URL.Path,
	}

	msg := &chat.Message{
		Channel: chat.ChannelRef{ID: r.URL.Path},
		Author:  chat.Author{Bot: true},
	}

	switch {
	case isConnectorCard:
		meta["generation"] = "connector"

		var card messageCard
		// sniff already proved the body is valid JSON, so this cannot fail.
		_ = json.Unmarshal(body, &card)

		if card.AtContext != "" {
			meta["@context"] = card.AtContext
		}
		if card.ThemeColor != "" {
			meta["themeColor"] = card.ThemeColor
		}
		if !isEmptyRaw(card.PotentialAction) {
			meta["potentialAction"] = card.PotentialAction
		}

		msg.Text = card.Text
		if msg.Text == "" {
			msg.Text = card.Summary
		}
		if len(card.Sections) > 0 {
			first := card.Sections[0]
			msg.Author.Name = first.ActivityTitle
			msg.Author.IconURL = first.ActivityImage
		}

		// The MessageCard payload has no envelope: the whole request body is
		// the card, so it is kept verbatim as-is (trimmed of the insignificant
		// whitespace the wire framing added around it).
		msg.Contents = append(msg.Contents, chat.Content{
			Format: chat.FormatTeamsMessageCard,
			Data:   json.RawMessage(bytes.TrimSpace(body)),
		})

	case len(sniff.Attachments) > 0:
		meta["generation"] = "workflow"
		meta["attachment_count"] = len(sniff.Attachments)

		for _, a := range sniff.Attachments {
			if isEmptyRaw(a.Content) {
				continue
			}
			format := chat.Format("")
			if strings.EqualFold(a.ContentType, adaptiveCardContentType) {
				format = chat.FormatTeamsAdaptiveCard
			}
			// a.Content is a json.RawMessage decoded straight out of the
			// request body: Unmarshal copies the exact input bytes for that
			// value, so this is the inner content object byte for byte, never
			// re-marshaled, and never the {"type":"message","attachments":…}
			// envelope around it.
			msg.Contents = append(msg.Contents, chat.Content{Format: format, Data: a.Content})
		}
		msg.Text = sniff.Text
		if msg.Text == "" {
			msg.Text = sniff.Summary
		}
		// No top-level text: Normalize (called by chat.NewEvent) derives one
		// from the card body's own TextBlock/text values via FallbackText.

	case strings.TrimSpace(sniff.Text) != "" || strings.TrimSpace(sniff.Summary) != "":
		// The bare {"text":"..."} shape a workflow trigger also accepts
		// directly, with no card at all.
		meta["generation"] = "workflow"
		msg.Text = sniff.Text
		if msg.Text == "" {
			msg.Text = sniff.Summary
		}

	default:
		// Mirrors the real endpoint's own validation error: a payload that is
		// valid JSON but carries no card and no text/summary is rejected
		// rather than stored as an empty message.
		writeError(w, http.StatusBadRequest, "Summary or Text is required.")
		return
	}

	ev := chat.NewEvent(ProviderName, msg)
	ev.Meta = meta
	ev.Raw = event.Raw{
		Transport: "http",
		PeerAddr:  r.RemoteAddr,
		Method:    r.Method,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		Body:      body,
		Text:      true,
	}

	if err := d.Append(ctx, ev); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if isConnectorCard {
		// The retired connector webhook's own success response: the literal
		// digit 1, as plain text.
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("1"))
		return
	}

	// A Power Automate workflow trigger answers 202 with an empty body.
	w.WriteHeader(http.StatusAccepted)
}
