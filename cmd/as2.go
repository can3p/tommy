// cmd/as2.go is the as2 half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and
// bootstrap helper - both live in cmd/mail.go since they are plugin-agnostic
// - and adds only what is specific to as2: the one provider that exists, how
// to build the plugin, and the CLI flags for the http provider's own
// settings.
//
// Only one provider ships: http, the RFC 4130 binding. There is no
// equivalent of mail/sms's --<provider>-api-key to pin a vendor credential,
// because AS2 has no vendor: the eight settings below are all about identity
// (which certificate tommy answers with and which one it trusts) rather than
// an API key to pin.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
	"github.com/spf13/cobra"
)

var as2Flags singlePluginFlags

// as2HTTPOptionFlags are the http provider's own CLI flags - the counterpart
// of [plugins.as2.providers.http] in tommy.toml.
//
// certFile/keyFile exist because tommy may be run inside a container in a
// cluster that already has its own CA, and the operator needs to hand it an
// identity the rest of the system already trusts rather than have it mint
// one nobody knows. inMemory is the opposite case: for anyone who wants
// tommy to leave no files behind, at the price of a partner having to
// re-import the certificate after every restart.
//
// asTo is not a rejection pin, and that is the interesting one. RFC 4130
// §6.2 says: "There is no required response to a client request containing
// invalid or unknown AS2-From or AS2-To header values." So a mismatch here
// is captured, answered with a normal MDN, and recorded on the event as
// as2_to_expected/as2_to_matched - you *see* the misconfiguration rather
// than getting a refusal. Do not "fix" this into a 403; the RFC declines to
// specify one.
//
// partnerCertFile is what turns "this signature is intact" into "this
// signature is from the partner I expect" - without it only the first of
// those two claims is ever proven (see plugins/as2/README.md, rule 3).
//
// maxBody has no port to sit next to: like mailjet and sendgrid, http is
// path-routed onto the one shared ingress rather than owning a listener of
// its own.
type as2HTTPOptionFlags struct {
	certFile        string
	keyFile         string
	partnerCertFile string
	certDir         string
	commonName      string
	inMemory        bool
	as2To           string
	maxBody         int
}

var as2HTTPFlags as2HTTPOptionFlags

func registerAS2HTTPOptionFlags(cmd *cobra.Command, f *as2HTTPOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.certFile, "as2-cert-file", "",
		"PEM certificate to answer with instead of one tommy generates; must be given together with --as2-key-file")
	fl.StringVar(&f.keyFile, "as2-key-file", "",
		"the unencrypted PEM private key for --as2-cert-file (an encrypted key has no terminal to prompt on)")
	fl.StringVar(&f.partnerCertFile, "as2-partner-cert-file", "",
		"PEM certificate inbound signatures are checked against; without it a signature is only proven intact, never proven to be from the partner you expect")
	fl.StringVar(&f.certDir, "as2-cert-dir", "",
		"directory a generated certificate is written to and reused from (default: beside the config file, or the OS user config dir)")
	fl.StringVar(&f.commonName, "as2-common-name", "",
		"subject CN of a generated certificate (ignored when --as2-cert-file is given)")
	fl.BoolVar(&f.inMemory, "as2-in-memory", false,
		"generate a key pair and write nothing to disk at all; a partner has to re-import the certificate after every restart")
	fl.StringVar(&f.as2To, "as2-to", "",
		"the AS2-To identifier this endpoint answers to (unset accepts anything); a mismatch is still captured and answered, never refused - RFC 4130 §6.2 requires no particular response to one")
	fl.IntVar(&f.maxBody, "as2-max-body", 0,
		"request body cap in bytes (0 leaves the provider's own default, 64MiB)")
}

// as2Providers returns fresh instances of every as2 provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func as2Providers() []plugin.Provider {
	return []plugin.Provider{as2http.New()}
}

var as2Cmd = &cobra.Command{
	Use:   "as2",
	Short: "Run only the as2 plugin: http",
	Long: `Run tommy with just the as2 plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch AS2 (RFC 4130) EDIINT exchanges.

  tommy as2 --ui-port 8811 --in-port 8822

An AS2 exchange starts with the partner fetching GET /as2/certificate -
tommy generates a certificate on first use rather than at startup, so that
route works before any message has arrived and a provider that is switched
off never causes one to be minted at all.

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only as2, and runs it through the
exact same bootstrap. With no --enabled-providers every as2 provider this
binary ships is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := as2Providers()
		opts := newProviderOptionBuilder(cmd)
		opts.set(as2http.Name, "as2-cert-file", "cert_file", as2HTTPFlags.certFile)
		opts.set(as2http.Name, "as2-key-file", "key_file", as2HTTPFlags.keyFile)
		opts.set(as2http.Name, "as2-partner-cert-file", "partner_cert_file", as2HTTPFlags.partnerCertFile)
		opts.set(as2http.Name, "as2-cert-dir", "cert_dir", as2HTTPFlags.certDir)
		opts.set(as2http.Name, "as2-common-name", "common_name", as2HTTPFlags.commonName)
		opts.set(as2http.Name, "as2-in-memory", "in_memory", as2HTTPFlags.inMemory)
		opts.set(as2http.Name, "as2-to", "as2_to", as2HTTPFlags.as2To)
		opts.set(as2http.Name, "as2-max-body", "max_body", as2HTTPFlags.maxBody)
		return runSinglePlugin(cmd, as2.Name, func() plugin.Plugin {
			return as2.New(as2Providers()...)
		}, providerNames(providers), as2Flags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(as2Cmd, &as2Flags)
	registerAS2HTTPOptionFlags(as2Cmd, &as2HTTPFlags)
	rootCmd.AddCommand(as2Cmd)
}
