// Package all is the single shared wiring point: it names every plugin this
// binary ships, and every provider inside it. Registration is explicit rather
// than init() magic, so what runs is decided in one readable file.
package all

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/msteams"
	"github.com/can3p/tommy/plugins/chat/providers/slack"
	"github.com/can3p/tommy/plugins/chat/ui/blocks"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/ftp"
	"github.com/can3p/tommy/plugins/files/providers/nfs"
	"github.com/can3p/tommy/plugins/files/providers/sftp"
	"github.com/can3p/tommy/plugins/files/providers/tftp"
	"github.com/can3p/tommy/plugins/hl7"
	"github.com/can3p/tommy/plugins/hl7/providers/mllp"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/mailjet"
	"github.com/can3p/tommy/plugins/mail/providers/sendgrid"
	"github.com/can3p/tommy/plugins/mail/providers/smtp"
	"github.com/can3p/tommy/plugins/push"
	"github.com/can3p/tommy/plugins/push/providers/apns"
	"github.com/can3p/tommy/plugins/push/providers/fcm"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
	"github.com/can3p/tommy/plugins/snmp"
	"github.com/can3p/tommy/plugins/snmp/providers/trap"
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
			tftp.New(),
			nfs.New(),
		),
		chat.New(
			slack.New(),
			msteams.New(),
		).WithRichRenderer(blocks.Render),
		hl7.New(
			mllp.New(),
		),
		snmp.New(
			trap.New(),
		),
		push.New(
			fcm.New(),
			apns.New(),
		),
		as2.New(
			as2http.New(),
		),
	}
}
