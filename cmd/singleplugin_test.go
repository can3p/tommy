// cmd/singleplugin_test.go covers the config-building CLI-1 added on top of
// cmd/mail.go's shared single-plugin helpers: provider-specific flags
// (--smtp-port, --ftp-passive-ports, --sftp-host-key, ...) landing in the
// right provider section with the right Go type, an unset flag never
// overriding a provider's own default, an unknown --enabled-providers name
// being rejected before anything binds, and a provider flag conflicting with
// --enabled-providers being rejected too.
package cmd

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/files/providers/ftp"
	"github.com/can3p/tommy/plugins/files/providers/sftp"
	"github.com/can3p/tommy/plugins/mail/providers/mailjet"
	"github.com/can3p/tommy/plugins/mail/providers/sendgrid"
	"github.com/can3p/tommy/plugins/mail/providers/smtp"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
	"github.com/spf13/cobra"
)

func baseSinglePluginFlags() singlePluginFlags {
	return singlePluginFlags{uiPort: -1, apiPort: -1, inPort: -1, logLevel: "info"}
}

// TestSinglePluginConfigProviderOptions checks that providerOptions land in
// the right provider's section, with an integer landing as an int (the
// TOML-shaped type config.ProviderConfig.Int expects), and that a provider
// named in allProviders but absent from providerOptions gets only
// "enabled": true - no stray keys.
func TestSinglePluginConfigProviderOptions(t *testing.T) {
	cfg, err := singlePluginConfig("mail", []string{"smtp", "mailjet"}, baseSinglePluginFlags(), map[string]map[string]any{
		"smtp": {"port": 1234, "username": "alice"},
	})
	if err != nil {
		t.Fatalf("singlePluginConfig: %v", err)
	}

	smtpCfg := cfg.Provider("mail", "smtp")
	if smtpCfg.Port != 1234 {
		t.Errorf("smtp port = %d, want 1234", smtpCfg.Port)
	}
	if got := smtpCfg.String("username", ""); got != "alice" {
		t.Errorf("smtp username = %q, want alice", got)
	}

	mailjetCfg := cfg.Provider("mail", "mailjet")
	if !mailjetCfg.Bool("enabled", false) {
		t.Error("mailjet should be enabled")
	}
	if _, ok := mailjetCfg.Get("port"); ok {
		t.Error("mailjet should have no port override - only smtp got provider options")
	}
}

// TestSinglePluginConfigUnsetFlagLeavesDefaultAlone drives the actual
// registered flags of cmd/files.go's ftp provider through cobra without
// setting --ftp-passive-host, then checks the value ftp.LoadConfig reads back
// is still ftp.DefaultPassiveHost - proving an unset flag never overrides the
// provider's own default, all the way through to the provider's own config
// loader.
func TestSinglePluginConfigUnsetFlagLeavesDefaultAlone(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var f ftpOptionFlags
	registerFTPOptionFlags(cmd, &f)
	if err := cmd.ParseFlags([]string{"--ftp-port", "2200"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(ftp.ProviderName, "ftp-port", "port", f.port)
	opts.set(ftp.ProviderName, "ftp-passive-host", "passive_host", f.passiveHost)
	opts.set(ftp.ProviderName, "ftp-passive-ports", "passive_ports", f.passivePorts)

	cfg, err := singlePluginConfig("files", []string{"ftp", "sftp"}, baseSinglePluginFlags(), opts.options)
	if err != nil {
		t.Fatalf("singlePluginConfig: %v", err)
	}

	provCfg, err := ftp.LoadConfig(cfg.Provider("files", "ftp"))
	if err != nil {
		t.Fatalf("ftp.LoadConfig: %v", err)
	}
	if provCfg.Port != 2200 {
		t.Errorf("port = %d, want 2200 (the flag that was set)", provCfg.Port)
	}
	if provCfg.PassiveHost != ftp.DefaultPassiveHost {
		t.Errorf("passive host = %q, want the untouched default %q", provCfg.PassiveHost, ftp.DefaultPassiveHost)
	}
	if provCfg.PassiveRange != nil {
		t.Errorf("passive range = %+v, want nil (no --ftp-passive-ports given)", provCfg.PassiveRange)
	}
}

// TestFilesProviderFlagsLandInRightSection exercises cmd/files.go's own flag
// registration end to end: parsing a mix of ftp and sftp flags must produce
// options keyed under the right provider name, one map per provider, never
// crossed.
func TestFilesProviderFlagsLandInRightSection(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var ff ftpOptionFlags
	var sf sftpOptionFlags
	registerFTPOptionFlags(cmd, &ff)
	registerSFTPOptionFlags(cmd, &sf)

	if err := cmd.ParseFlags([]string{
		"--ftp-port", "2200",
		"--ftp-passive-ports", "50000-50100",
		"--sftp-port", "2300",
		"--sftp-host-key", "/tmp/does-not-matter",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(ftp.ProviderName, "ftp-port", "port", ff.port)
	opts.set(ftp.ProviderName, "ftp-passive-host", "passive_host", ff.passiveHost)
	opts.set(ftp.ProviderName, "ftp-passive-ports", "passive_ports", ff.passivePorts)
	opts.set(ftp.ProviderName, "ftp-username", "username", ff.username)
	opts.set(ftp.ProviderName, "ftp-password", "password", ff.password)
	opts.set(sftp.ProviderName, "sftp-port", "port", sf.port)
	opts.set(sftp.ProviderName, "sftp-host-key", "host_key_path", sf.hostKeyPath)
	opts.set(sftp.ProviderName, "sftp-authorized-keys", "authorized_keys", sf.authorizedKeys)
	opts.set(sftp.ProviderName, "sftp-username", "username", sf.username)
	opts.set(sftp.ProviderName, "sftp-password", "password", sf.password)

	ftpOpts, ok := opts.options["ftp"]
	if !ok {
		t.Fatal("no options recorded for ftp")
	}
	if len(ftpOpts) != 2 {
		t.Errorf("ftp options = %+v, want exactly port and passive_ports", ftpOpts)
	}
	if ftpOpts["port"] != 2200 {
		t.Errorf("ftp port = %v, want int 2200", ftpOpts["port"])
	}
	if ftpOpts["passive_ports"] != "50000-50100" {
		t.Errorf("ftp passive_ports = %v", ftpOpts["passive_ports"])
	}

	sftpOpts, ok := opts.options["sftp"]
	if !ok {
		t.Fatal("no options recorded for sftp")
	}
	if len(sftpOpts) != 2 {
		t.Errorf("sftp options = %+v, want exactly port and host_key_path", sftpOpts)
	}
	if sftpOpts["port"] != 2300 {
		t.Errorf("sftp port = %v, want int 2300", sftpOpts["port"])
	}
	if sftpOpts["host_key_path"] != "/tmp/does-not-matter" {
		t.Errorf("sftp host_key_path = %v", sftpOpts["host_key_path"])
	}
}

// TestMailSMTPFlagsLandInSMTPSection is cmd/mail.go's equivalent of the ftp
// case above.
func TestMailSMTPFlagsLandInSMTPSection(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var f smtpOptionFlags
	registerSMTPOptionFlags(cmd, &f)
	if err := cmd.ParseFlags([]string{"--smtp-port", "0", "--smtp-username", "alice"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(smtp.ProviderName, "smtp-port", "port", f.port)
	opts.set(smtp.ProviderName, "smtp-username", "username", f.username)
	opts.set(smtp.ProviderName, "smtp-password", "password", f.password)

	smtpOpts, ok := opts.options["smtp"]
	if !ok {
		t.Fatal("no options recorded for smtp")
	}
	if len(smtpOpts) != 2 {
		t.Errorf("smtp options = %+v, want exactly port and username", smtpOpts)
	}
	if smtpOpts["port"] != 0 {
		t.Errorf("smtp port = %v, want int 0 (--smtp-port 0 was given explicitly)", smtpOpts["port"])
	}
	if smtpOpts["username"] != "alice" {
		t.Errorf("smtp username = %v", smtpOpts["username"])
	}
	if _, ok := smtpOpts["password"]; ok {
		t.Error("password should be absent - its flag was never given")
	}
}

// TestSinglePluginConfigUnknownProviderNamesValid checks that an unknown
// --enabled-providers name is rejected before anything is built, and that the
// error names the valid providers so the user does not have to go read the
// source to find them.
func TestSinglePluginConfigUnknownProviderNamesValid(t *testing.T) {
	f := baseSinglePluginFlags()
	f.enabledProviders = "bogus"

	_, err := singlePluginConfig("mail", []string{"smtp", "mailjet"}, f, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("err = %v, want it to name the bad value", err)
	}
	for _, want := range []string{"smtp", "mailjet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name valid provider %q", err, want)
		}
	}
}

// TestSinglePluginConfigProviderOptionConflict checks that a provider-option
// flag set for a provider --enabled-providers excludes is rejected with a
// clear error naming the conflict, rather than the flag being silently
// dropped.
func TestSinglePluginConfigProviderOptionConflict(t *testing.T) {
	f := baseSinglePluginFlags()
	f.enabledProviders = "mailjet"

	_, err := singlePluginConfig("mail", []string{"smtp", "mailjet"}, f, map[string]map[string]any{
		"smtp": {"port": 1234},
	})
	if err == nil {
		t.Fatal("expected an error: smtp flags were given but --enabled-providers only enables mailjet")
	}
	if !strings.Contains(err.Error(), "smtp") || !strings.Contains(err.Error(), "mailjet") {
		t.Errorf("err = %v, want it to name both the excluded provider and what is enabled", err)
	}
}

// TestMailjetAndSendgridFlagsLandInRightSection is cmd/mail.go's HTTP-provider
// equivalent of TestFilesProviderFlagsLandInRightSection: pinning a vendor
// credential through a flag is the same error-path test as pinning smtp's
// AUTH, and this checks the flags feed the right key into the right
// provider's section, never crossed with each other or with smtp. Neither
// provider gets a --<name>-port flag: core has no per-provider-listener
// mechanism for an HTTP provider (core/config/provider.go's Port field is
// only ever read as a fallback for reporting where a *listener* provider
// bound, and core/server/ingress/mount.go path-routes every HTTP provider
// onto the one shared ingress with no notion of a per-provider port), so a
// port flag here would set a config key nothing reads.
func TestMailjetAndSendgridFlagsLandInRightSection(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var mj mailjetOptionFlags
	var sg sendgridOptionFlags
	registerMailjetOptionFlags(cmd, &mj)
	registerSendgridOptionFlags(cmd, &sg)

	if err := cmd.ParseFlags([]string{
		"--mailjet-api-key", "mj-key",
		"--mailjet-secret-key", "mj-secret",
		"--sendgrid-api-key", "sg-key",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(mailjet.ProviderName, "mailjet-api-key", "api_key", mj.apiKey)
	opts.set(mailjet.ProviderName, "mailjet-secret-key", "secret_key", mj.secretKey)
	opts.set(sendgrid.ProviderName, "sendgrid-api-key", "api_key", sg.apiKey)

	mjOpts, ok := opts.options["mailjet"]
	if !ok {
		t.Fatal("no options recorded for mailjet")
	}
	if len(mjOpts) != 2 {
		t.Errorf("mailjet options = %+v, want exactly api_key and secret_key", mjOpts)
	}
	if mjOpts["api_key"] != "mj-key" {
		t.Errorf("mailjet api_key = %v", mjOpts["api_key"])
	}
	if mjOpts["secret_key"] != "mj-secret" {
		t.Errorf("mailjet secret_key = %v", mjOpts["secret_key"])
	}

	sgOpts, ok := opts.options["sendgrid"]
	if !ok {
		t.Fatal("no options recorded for sendgrid")
	}
	if len(sgOpts) != 1 || sgOpts["api_key"] != "sg-key" {
		t.Errorf("sendgrid options = %+v, want exactly api_key=sg-key", sgOpts)
	}
}

// TestTwilioFlagsLandInTwilioSection is cmd/sms.go's equivalent of the
// mailjet case above - twilio gets no --twilio-port for the same reason.
func TestTwilioFlagsLandInTwilioSection(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var tw twilioOptionFlags
	registerTwilioOptionFlags(cmd, &tw)

	if err := cmd.ParseFlags([]string{"--twilio-account-sid", "AC0000", "--twilio-auth-token", "tok"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := newProviderOptionBuilder(cmd)
	opts.set(twilio.Name, "twilio-account-sid", "account_sid", tw.accountSid)
	opts.set(twilio.Name, "twilio-auth-token", "auth_token", tw.authToken)

	twOpts, ok := opts.options["twilio"]
	if !ok {
		t.Fatal("no options recorded for twilio")
	}
	if len(twOpts) != 2 {
		t.Errorf("twilio options = %+v, want exactly account_sid and auth_token", twOpts)
	}
	if twOpts["account_sid"] != "AC0000" || twOpts["auth_token"] != "tok" {
		t.Errorf("twilio options = %+v", twOpts)
	}
}

// TestSinglePluginCommandsRejectStrayArgs is the regression test for the
// "tommy mail help" papercut: none of the single-plugin shortcuts, nor
// serve, used to set Args, so cobra silently handed an unrecognized
// positional argument to RunE and it was ignored - "tommy mail help
// --ui-port 0 ..." booted a real server instead of erroring. Every command
// here must reject a stray positional argument before RunE ever runs.
func TestSinglePluginCommandsRejectStrayArgs(t *testing.T) {
	cmds := map[string]*cobra.Command{
		"mail":  mailCmd,
		"sms":   smsCmd,
		"files": filesCmd,
		"chat":  chatCmd,
		"serve": serveCmd,
	}
	for name, cmd := range cmds {
		t.Run(name, func(t *testing.T) {
			if cmd.Args == nil {
				t.Fatalf("%s has no Args validator: a stray positional argument would silently reach RunE", name)
			}
			if err := cmd.Args(cmd, []string{"help"}); err == nil {
				t.Errorf("%s accepted a stray positional argument instead of erroring", name)
			}
		})
	}
}
