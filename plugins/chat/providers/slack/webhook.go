package slack

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/chat"
)

// webhookPayload is the JSON body an incoming webhook POST carries. Slack's
// current-generation apps reject channel/username/icon_url/icon_emoji
// overrides on this surface, but plenty of real-world payloads - and the
// legacy "custom integration" webhooks Slack ran for years - still send them,
// so this provider accepts and records them rather than silently dropping
// fields a test's fixture might depend on.
type webhookPayload struct {
	Text        string          `json:"text"`
	Blocks      json.RawMessage `json:"blocks"`
	Attachments json.RawMessage `json:"attachments"`
	Channel     string          `json:"channel"`
	Username    string          `json:"username"`
	IconURL     string          `json:"icon_url"`
	IconEmoji   string          `json:"icon_emoji"`
	ThreadTS    string          `json:"thread_ts"`
	Mrkdwn      *bool           `json:"mrkdwn"`
	UnfurlLinks *bool           `json:"unfurl_links"`
	UnfurlMedia *bool           `json:"unfurl_media"`
}

// webhook handles POST /services/{team}/{bot}/{token}: Slack's incoming
// webhook contract. The body is always JSON on this surface.
func (p *Provider) webhook(d plugin.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		team := r.PathValue("team")
		bot := r.PathValue("bot")
		token := r.PathValue("token")

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeWebhookError(w, http.StatusBadRequest, "invalid_payload")
			return
		}

		var payload webhookPayload
		if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
			writeWebhookError(w, http.StatusBadRequest, "invalid_payload")
			return
		}

		c := content{
			Text:        payload.Text,
			Channel:     payload.Channel,
			Username:    payload.Username,
			IconURL:     payload.IconURL,
			IconEmoji:   payload.IconEmoji,
			ThreadTS:    payload.ThreadTS,
			Blocks:      payload.Blocks,
			Attachments: payload.Attachments,
			Mrkdwn:      payload.Mrkdwn,
			UnfurlLinks: payload.UnfurlLinks,
			UnfurlMedia: payload.UnfurlMedia,
		}
		if !c.hasBody() {
			writeWebhookError(w, http.StatusBadRequest, "no_text")
			return
		}

		// An incoming webhook never returns a ts to the poster - Slack posts it
		// fire-and-forget - so the message carries none of its own; its identity
		// falls back to the event id, exactly as message.go documents.
		msg := buildMessage(c, webhookChannelRef(payload.Channel, team, bot), "")

		meta := map[string]any{
			"team":          team,
			"bot":           bot,
			"webhook_token": token,
		}
		strMeta(meta, "icon_emoji", payload.IconEmoji)
		boolMeta(meta, "mrkdwn", payload.Mrkdwn)
		boolMeta(meta, "unfurl_links", payload.UnfurlLinks)
		boolMeta(meta, "unfurl_media", payload.UnfurlMedia)

		ev := chat.NewEvent(Name, msg)
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
		if err := d.Append(r.Context(), ev); err != nil {
			writeWebhookError(w, http.StatusInternalServerError, "internal_error")
			return
		}

		writeWebhookOK(w)
	}
}
