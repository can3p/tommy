package mailjet

import (
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/plugin"
)

// validationError is one message's worth of trouble. It carries everything
// needed to build the per-message errorResult that rides inside the batch's
// 200 response, per Mailjet's documented partial-failure shape.
type validationError struct {
	code      string
	status    int
	message   string
	relatedTo []string
}

func newValidationError(code string, status int, message string, relatedTo []string) *validationError {
	return &validationError{code: code, status: status, message: message, relatedTo: relatedTo}
}

// result builds the wire-shaped errorResult. d is unused today but kept so a
// future switch to Deps.NewID for ErrorIdentifier - for fully deterministic
// golden bytes - is a one-line change here rather than a signature change
// throughout the package.
func (v *validationError) result(_ plugin.Deps) errorResult {
	return errorResult{
		Status: "error",
		Errors: []detailError{{
			ErrorIdentifier: newUUID(),
			ErrorCode:       v.code,
			StatusCode:      v.status,
			ErrorMessage:    v.message,
			ErrorRelatedTo:  v.relatedTo,
		}},
	}
}

// validateMessage checks the handful of properties Mailjet itself documents as
// required, stopping at the first problem - real Mailjet can return several
// errors at once for one message, but one is enough for tommy to prove the
// shape is right.
func validateMessage(wm wireMessage) *validationError {
	if wm.From.Email == "" {
		return newValidationError("mj-0004", http.StatusBadRequest,
			`"From" property is required and must contain a valid "Email".`, []string{"From"})
	}
	if len(wm.To)+len(wm.Cc)+len(wm.Bcc) == 0 {
		return newValidationError("mj-0004", http.StatusBadRequest,
			`At least one recipient ("To", "Cc" or "Bcc") is required.`, []string{"To", "Cc", "Bcc"})
	}
	if strings.TrimSpace(wm.TextPart) == "" && strings.TrimSpace(wm.HTMLPart) == "" {
		// Verified verbatim against
		// https://dev.mailjet.com/docs/email-api/send-api-v31/send-api-errors's
		// documented example error, ErrorCode "send-0003".
		return newValidationError("send-0003", http.StatusBadRequest,
			`At least "HTMLPart", "TextPart" or "TemplateID" must be provided.`, []string{"HTMLPart", "TextPart"})
	}
	return nil
}
