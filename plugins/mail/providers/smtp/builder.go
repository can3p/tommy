package smtp

import (
	"context"
	"errors"
	"strings"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/plugins/mail"
)

// builder walks the MIME tree and fills in the canonical message: which part is
// the text body, which is the HTML body, and which parts are attachments.
type builder struct {
	ctx   context.Context
	blobs blob.BlobStore
	msg   *mail.Message
	p     *parser
	err   error
}

// walk descends one part. inRelated says whether the enclosing container was a
// multipart/related, where a part carrying a Content-ID is an image the HTML
// body references rather than a download.
func (b *builder) walk(pt *part, inRelated bool) {
	if b.err != nil || pt == nil {
		return
	}

	switch {
	case pt.mediaType == "multipart/alternative" && pt.multipart:
		// The alternatives are the same content in rising order of richness, so
		// a later sibling replaces an earlier one - but only within this
		// container, never a body found elsewhere in the message.
		var setText, setHTML bool
		for _, child := range pt.children {
			switch {
			case child.multipart:
				b.walk(child, inRelated)
			case child.mediaType == "text/plain" && !b.isAttachment(child, inRelated):
				b.setText(b.partText(child), setText)
				setText = true
			case child.mediaType == "text/html" && !b.isAttachment(child, inRelated):
				b.setHTML(b.partText(child), setHTML)
				setHTML = true
			default:
				b.leaf(child, inRelated)
			}
		}

	case pt.multipart:
		related := inRelated || pt.mediaType == "multipart/related"
		for _, child := range pt.children {
			b.walk(child, related)
		}

	default:
		b.leaf(pt, inRelated)
	}
}

// leaf turns one non-multipart part into a body or an attachment.
func (b *builder) leaf(pt *part, inRelated bool) {
	if b.isAttachment(pt, inRelated) {
		b.attach(pt, inRelated)
		return
	}
	switch pt.mediaType {
	case "text/plain":
		if b.msg.Text == "" {
			b.setText(b.partText(pt), false)
			return
		}
	case "text/html":
		if b.msg.HTML == "" {
			b.setHTML(b.partText(pt), false)
			return
		}
	}
	// A body part the message already has - a second text/plain in a mixed
	// container, say - is kept as an attachment rather than thrown away.
	b.attach(pt, inRelated)
}

// isAttachment decides whether a part is a file rather than a body.
func (b *builder) isAttachment(pt *part, inRelated bool) bool {
	disp, _ := b.disposition(pt)
	switch disp {
	case "attachment":
		return true
	case "inline":
		// An inline part is still a file when it is not text, or when it is
		// named, or when the HTML body refers to it by cid.
		return b.filename(pt) != "" || contentID(pt) != "" || !isBodyType(pt.mediaType)
	}
	if !isBodyType(pt.mediaType) {
		return true
	}
	if b.filename(pt) != "" {
		return true
	}
	return inRelated && contentID(pt) != ""
}

// isBodyType reports whether a media type can serve as a message body.
func isBodyType(mediaType string) bool {
	return mediaType == "text/plain" || mediaType == "text/html"
}

func (b *builder) setText(s string, overwrite bool) {
	if overwrite || b.msg.Text == "" {
		b.msg.Text = s
	}
}

func (b *builder) setHTML(s string, overwrite bool) {
	if overwrite || b.msg.HTML == "" {
		b.msg.HTML = s
	}
}

// partText decodes a text part's bytes into a string using its charset, and
// normalizes the wire's CRLF line endings. The untouched bytes stay in
// event.Raw; what lands in the canonical message is text a template can render.
func (b *builder) partText(pt *part) string {
	return strings.ReplaceAll(b.p.text(pt.params["charset"], pt.content), "\r\n", "\n")
}

// disposition returns the Content-Disposition type and its parameters.
func (b *builder) disposition(pt *part) (string, map[string]string) {
	value := pt.header.Get("Content-Disposition")
	if strings.TrimSpace(value) == "" {
		return "", map[string]string{}
	}
	disp, params := b.p.mediaType(value, "")
	return strings.ToLower(strings.TrimSpace(disp)), params
}

// filename returns the name a part wants to be saved under, from either the
// Content-Disposition filename (RFC 2231 continuations and charset included,
// courtesy of mime.ParseMediaType) or the legacy Content-Type name parameter.
func (b *builder) filename(pt *part) string {
	_, params := b.disposition(pt)
	name := params["filename"]
	if name == "" {
		name = pt.params["name"]
	}
	if name == "" {
		return ""
	}
	// Some clients wrongly put an RFC 2047 encoded word where RFC 2231 belongs.
	name = b.p.decodeHeader(name)
	return sanitizeFilename(name)
}

// sanitizeFilename keeps a filename a filename: no directories, no control
// characters. The bytes never touch a filesystem, but the name is echoed into a
// Content-Disposition header on the way back out.
func sanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

func contentID(pt *part) string {
	return mail.TrimContentID(pt.header.Get("Content-ID"))
}

// attach stores a part's bytes in the blob store and records the attachment.
//
// A blob store that is full is not a reason to lose the message: the mail is
// still recorded, with a warning saying which attachment did not fit.
func (b *builder) attach(pt *part, inRelated bool) {
	disp, _ := b.disposition(pt)
	cid := contentID(pt)
	att := mail.Attachment{
		Filename:    b.filename(pt),
		ContentType: attachmentContentType(pt),
		ContentID:   cid,
		Inline:      disp == "inline" || (disp == "" && cid != "" && inRelated),
	}
	if _, err := b.msg.AttachBytes(b.ctx, b.blobs, att, pt.content); err != nil {
		if errors.Is(err, blob.ErrCapacityExceeded) {
			b.p.warn("attachment %q did not fit in the blob store and was dropped", att.Name())
			return
		}
		b.err = err
	}
}

// attachmentContentType keeps the charset of a text attachment, which is the
// only parameter that changes how the bytes read back.
func attachmentContentType(pt *part) string {
	ct := pt.mediaType
	if strings.HasPrefix(ct, "text/") {
		if cs := pt.params["charset"]; cs != "" {
			ct += "; charset=" + cs
		}
	}
	return ct
}
