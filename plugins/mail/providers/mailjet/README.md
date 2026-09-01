# mailjet

Imitates [Mailjet's transactional Send API v3.1](https://dev.mailjet.com/email/guides/send-api-v31/).

Mounts `POST /v3.1/send` on the shared ingress. Accepts the real
`{"Messages":[...]}` batch shape, including `Attachments` /
`InlinedAttachments` (`Base64Content` decoded into the blob store),
`ReplyTo`, `Headers`, `CustomID`, `EventPayload`, `CustomCampaign` and
`SandboxMode`. Basic-auth credentials are accepted by default and recorded on
the event; a provider config with `api_key` (and optionally `secret_key`) set
pins an expected pair and rejects a mismatch with Mailjet's real 401 shape.

Every entry of `Messages[]` is one delivered message: a request that fans out
to three logical messages appends three events, each one recipient's
`MessageID`/`MessageUUID` minted per-address, matching the real API's own
multi-recipient example.

## Try it

```bash
curl -s http://localhost:8822/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'
```

## Notes on the wire shapes

Verified against the live docs while implementing this provider:

- `ReplyTo` is a **single** `{"Email","Name"}` object, unlike `To`/`Cc`/`Bcc`.
- A per-message validation failure (e.g. missing `TextPart`/`HTMLPart`/`TemplateID`)
  rides inside a **200** response as `{"Status":"error","Errors":[...]}` at that
  message's position in `Messages[]`; only a malformed request as a whole (bad
  JSON, empty `Messages[]`, bad auth) is a top-level 4xx with the flat
  `{"ErrorIdentifier","ErrorCode","StatusCode","ErrorMessage"}` shape.
- In `SandboxMode`, the response omits `MessageID`/`MessageUUID` (`0`/`""`).
- `blob.ErrCapacityExceeded` is reported as a per-message error too (so a
  batch can't be aborted by one oversized attachment), but with a tommy-only
  `ErrorCode` - Mailjet itself has no such limit.
