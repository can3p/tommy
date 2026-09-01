// Package all is the single shared wiring point: it names every plugin this
// binary ships, and every provider inside it. Registration is explicit rather
// than init() magic, so what runs is decided in one readable file.
package all

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/ftp"
	"github.com/can3p/tommy/plugins/files/providers/sftp"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/mailjet"
	"github.com/can3p/tommy/plugins/mail/providers/sendgrid"
	"github.com/can3p/tommy/plugins/mail/providers/smtp"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
)

// Plugins returns every plugin compiled into this binary.
func Plugins() []plugin.Plugin {
	return []plugin.Plugin{
		mail.New(
			mailjet.New(),
			sendgrid.New(),
			smtp.New(),
		),
		sms.New(
			sms.WithProviders(twilio.New()),
		),
		files.New(
			ftp.New(),
			sftp.New(),
		),
	}
}
