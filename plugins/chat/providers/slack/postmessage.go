package slack

import (
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/chat"
)

// postMessageParams is what chat.postMessage accepts, decoded from either
// content type it supports.
type postMessageParams struct {
	Token          string
	Channel        string
	Text           string
	Blocks         json.RawMessage
	Attachments    json.RawMessage
	ThreadTS       string
	ReplyBroadcast *bool
	Username       string
	IconURL        string
	IconEmoji      string
	Mrkdwn         *bool
	UnfurlLinks    *bool
	UnfurlMedia    *bool
	AsUser         *bool
}

// postMessageJSON is the JSON body shape. blocks and attachments are captured
// as json.RawMessage so the exact bytes the caller sent are what gets stored.
type postMessageJSON struct {
	Token          string          `json:"token"`
	Channel        string          `json:"channel"`
	Text           string          `json:"text"`
	Blocks         json.RawMessage `json:"blocks"`
	Attachments    json.RawMessage `json:"attachments"`
	ThreadTS       string          `json:"thread_ts"`
	ReplyBroadcast *bool           `json:"reply_broadcast"`
	Username       string          `json:"username"`
	IconURL        string          `json:"icon_url"`
	IconEmoji      string          `json:"icon_emoji"`
	Mrkdwn         *bool           `json:"mrkdwn"`
	UnfurlLinks    *bool           `json:"unfurl_links"`
	UnfurlMedia    *bool           `json:"unfurl_media"`
	AsUser         *bool           `json:"as_user"`
}

// parsePostMessage decodes body as JSON when contentType says so, and as
// application/x-www-form-urlencoded otherwise - both are documented as
// accepted for this method. On the form path, blocks and attachments arrive as
// a JSON-encoded string (Slack's own documented shape for passing a structured
// argument over a form); url.ParseQuery already undoes the percent-encoding,
// so the string is used as the verbatim JSON bytes with no further decoding.
func parsePostMessage(contentType string, body []byte) (postMessageParams, bool) {
	if strings.Contains(contentType, "application/json") {
		var j postMessageJSON
		if err := json.Unmarshal(body, &j); err != nil {
			return postMessageParams{}, false
		}
		// postMessageParams and postMessageJSON share identical fields in the
		// same order - only the json tags differ - so this is a plain
		// conversion, not a field-by-field copy.
		return postMessageParams(j), true
	}

	values, err := url.ParseQuery(string(body))
	if err != nil {
		return postMessageParams{}, false
	}
	params := postMessageParams{
		Token:          values.Get("token"),
		Channel:        values.Get("channel"),
		Text:           values.Get("text"),
		ThreadTS:       values.Get("thread_ts"),
		ReplyBroadcast: parseBoolPtr(values.Get("reply_broadcast")),
		Username:       values.Get("username"),
		IconURL:        values.Get("icon_url"),
		IconEmoji:      values.Get("icon_emoji"),
		Mrkdwn:         parseBoolPtr(values.Get("mrkdwn")),
		UnfurlLinks:    parseBoolPtr(values.Get("unfurl_links")),
		UnfurlMedia:    parseBoolPtr(values.Get("unfurl_media")),
		AsUser:         parseBoolPtr(values.Get("as_user")),
	}
	if raw := values.Get("blocks"); raw != "" && json.Valid([]byte(raw)) {
		params.Blocks = json.RawMessage(raw)
	}
	if raw := values.Get("attachments"); raw != "" && json.Valid([]byte(raw)) {
		params.Attachments = json.RawMessage(raw)
	}
	return params, true
}

// parseBoolPtr reads one of the boolean-ish strings Slack's form fields
// accept. An empty or unrecognized value means "not set", not "false".
func parseBoolPtr(s string) *bool {
	switch s {
	case "true", "1":
		v := true
		return &v
	case "false", "0":
		v := false
		return &v
	default:
		return nil
	}
}

// bearerToken extracts the presented credential: the Authorization header
// takes priority, per Slack's own documented preference, falling back to the
// token carried in the body.
func bearerToken(r *http.Request, bodyToken string) (token string, presented bool) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(after), true
		}
		return strings.TrimSpace(auth), true
	}
	if bodyToken != "" {
		return bodyToken, true
	}
	return "", false
}

// authOK reports whether the presented token is acceptable. Like the Twilio
// provider, any token (or none) is accepted by default; pinning a bot_token in
// this provider's config section starts rejecting a mismatch with Slack's own
// invalid_auth error.
func authOK(d plugin.Deps, token string) bool {
	pinned := d.Config.String("bot_token", "")
	if pinned == "" {
		return true
	}
	return token == pinned
}

// fakeBotID mints a deterministic, Slack-shaped bot id ("B" followed by ten
// base-32-ish characters) from the presented token, so the same fake
// credential always answers as the same bot without tommy having to model
// app installation at all.
func fakeBotID(token string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(token))
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	sum := h.Sum32()
	b := make([]byte, 10)
	for i := range b {
		b[i] = alphabet[sum%32]
		sum /= 32
	}
	return "B" + string(b)
}

// messageOut is the "message" object chat.postMessage echoes back in its
// success envelope, verified against the live method reference.
type messageOut struct {
	Type        string          `json:"type"`
	Text        string          `json:"text"`
	Username    string          `json:"username,omitempty"`
	BotID       string          `json:"bot_id"`
	TS          string          `json:"ts"`
	ThreadTS    string          `json:"thread_ts,omitempty"`
	Blocks      json.RawMessage `json:"blocks,omitempty"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
}

// postMessage handles POST /api/chat.postMessage.
func (p *Provider) postMessage(d plugin.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeAPIError(w, "invalid_arguments")
			return
		}

		params, ok := parsePostMessage(r.Header.Get("Content-Type"), body)
		if !ok {
			writeAPIError(w, "invalid_arguments")
			return
		}

		token, presented := bearerToken(r, params.Token)
		switch {
		case !presented:
			writeAPIError(w, "not_authed")
			return
		case !authOK(d, token):
			writeAPIError(w, "invalid_auth")
			return
		}

		if strings.TrimSpace(params.Channel) == "" {
			writeAPIError(w, "channel_not_found")
			return
		}

		c := content{
			Text:        params.Text,
			Username:    params.Username,
			IconURL:     params.IconURL,
			IconEmoji:   params.IconEmoji,
			ThreadTS:    params.ThreadTS,
			Blocks:      params.Blocks,
			Attachments: params.Attachments,
			Mrkdwn:      params.Mrkdwn,
			UnfurlLinks: params.UnfurlLinks,
			UnfurlMedia: params.UnfurlMedia,
		}
		if !c.hasBody() {
			writeAPIError(w, "no_text")
			return
		}

		id := d.NewID()
		ts := mintTS(id, d.Now())
		msg := buildMessage(c, channelRef(params.Channel), ts)

		meta := map[string]any{
			"bearer_token":     token,
			"bearer_presented": presented,
			"generated_ts":     ts,
		}
		strMeta(meta, "icon_emoji", params.IconEmoji)
		boolMeta(meta, "mrkdwn", params.Mrkdwn)
		boolMeta(meta, "unfurl_links", params.UnfurlLinks)
		boolMeta(meta, "unfurl_media", params.UnfurlMedia)
		boolMeta(meta, "as_user", params.AsUser)
		boolMeta(meta, "reply_broadcast", params.ReplyBroadcast)

		ev := chat.NewEvent(Name, msg)
		ev.ID = event.ID(id)
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
			writeAPIError(w, "internal_error")
			return
		}

		botID := fakeBotID(token)
		echoBlocks, echoAttachments := params.Blocks, params.Attachments
		if !rawHasValue(echoBlocks) {
			echoBlocks = nil
		}
		if !rawHasValue(echoAttachments) {
			echoAttachments = nil
		}
		writeAPI(w, map[string]any{
			"ok":      true,
			"channel": msg.Channel.ID,
			"ts":      ts,
			"message": messageOut{
				Type:        "message",
				Text:        msg.Text,
				Username:    msg.Author.Name,
				BotID:       botID,
				TS:          ts,
				ThreadTS:    msg.ThreadTS,
				Blocks:      echoBlocks,
				Attachments: echoAttachments,
			},
		})
	}
}
