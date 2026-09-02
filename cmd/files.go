// cmd/files.go is the files half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and bootstrap
// helper - both live in cmd/mail.go since they are plugin-agnostic - and adds
// only what is specific to files: which providers exist, how to build the
// plugin, and the CLI flags for ftp's and sftp's own listeners.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/ftp"
	"github.com/can3p/tommy/plugins/files/providers/sftp"
	"github.com/can3p/tommy/plugins/files/providers/tftp"
	"github.com/spf13/cobra"
)

var filesFlags singlePluginFlags

// ftpOptionFlags are the ftp provider's own CLI flags - the counterpart of
// [plugins.files.providers.ftp] in tommy.toml. Port, the passive host/range
// and credentials get flags: the plan calls out the passive range explicitly
// (a client behind NAT or a firewall cannot connect without it), the passive
// host is the address that range is advertised against, and credentials
// decide whether login is checked or merely recorded. idle_timeout and
// connection_timeout are tuning knobs an application never needs to flip per
// test run, so they stay config-file-only - see tommy.toml's
// [plugins.files.providers.ftp] comments for that reasoning spelled out.
type ftpOptionFlags struct {
	port         int
	passiveHost  string
	passivePorts string
	username     string
	password     string
}

var filesFTPFlags ftpOptionFlags

func registerFTPOptionFlags(cmd *cobra.Command, f *ftpOptionFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.port, "ftp-port", ftp.DefaultPort, "port for the ftp provider's own listener (0 picks a free one)")
	fl.StringVar(&f.passiveHost, "ftp-passive-host", ftp.DefaultPassiveHost,
		"IPv4 address advertised to clients for passive-mode data connections")
	fl.StringVar(&f.passivePorts, "ftp-passive-ports", "",
		`restrict passive data connections to "START-END" (default: the OS picks one per transfer)`)
	fl.StringVar(&f.username, "ftp-username", "", "pin the username the ftp provider accepts (unset accepts any, or none)")
	fl.StringVar(&f.password, "ftp-password", "", "pin the password the ftp provider accepts")
}

// sftpOptionFlags are the sftp provider's own CLI flags - the counterpart of
// [plugins.files.providers.sftp] in tommy.toml. Port, the host key path and
// credentials get flags: the plan calls out the host key path explicitly
// (the identity must survive a restart or every client fails with a
// changed-host-key error), authorized_keys is how public-key auth is pinned,
// and username/password pin password auth. server_version and the
// handshake/idle timeouts and connection/auth-attempt caps are tuning knobs
// an application never needs to flip per test run, so they stay
// config-file-only - see tommy.toml's [plugins.files.providers.sftp]
// comments for that reasoning spelled out.
type sftpOptionFlags struct {
	port           int
	hostKeyPath    string
	authorizedKeys string
	username       string
	password       string
}

var filesSFTPFlags sftpOptionFlags

func registerSFTPOptionFlags(cmd *cobra.Command, f *sftpOptionFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.port, "sftp-port", sftp.DefaultPort, "port for the sftp provider's own listener (0 picks a free one)")
	fl.StringVar(&f.hostKeyPath, "sftp-host-key", sftp.DefaultHostKeyPath(),
		"path to the sftp provider's ed25519 host key (generated on first use and reused after)")
	fl.StringVar(&f.authorizedKeys, "sftp-authorized-keys", "",
		"an OpenSSH authorized_keys file to check public-key auth against (unset accepts any key)")
	fl.StringVar(&f.username, "sftp-username", "", "pin the password-auth username the sftp provider accepts")
	fl.StringVar(&f.password, "sftp-password", "", "pin the password-auth password the sftp provider accepts")
}

// tftpOptionFlags are the tftp provider's own CLI flags - the counterpart of
// [plugins.files.providers.tftp] in tommy.toml. Port is the only flag-worthy
// setting: TFTP (RFC 1350) has no login step at all, so there are no
// credentials to pin the way ftp's and sftp's flags do, and timeout_seconds /
// retries are tuning knobs an application never needs to flip per test run -
// see tommy.toml's [plugins.files.providers.tftp] comments for that
// reasoning spelled out, the same shape as ftp's and sftp's config-only
// knobs.
type tftpOptionFlags struct {
	port int
}

var filesTFTPFlags tftpOptionFlags

func registerTFTPOptionFlags(cmd *cobra.Command, f *tftpOptionFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.port, "tftp-port", tftp.DefaultPort, "port for the tftp provider's own listener (0 picks a free one)")
}

// filesProviders returns fresh instances of every files provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func filesProviders() []plugin.Provider {
	return []plugin.Provider{ftp.New(), sftp.New(), tftp.New()}
}

var filesCmd = &cobra.Command{
	Use:   "files",
	Short: "Run only the files plugin: ftp, sftp and tftp",
	Long: `Run tommy with just the files plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch uploaded files.

  tommy files --ui-port 8811 --in-port 8822 --ftp-port 2121 --sftp-port 2222 --tftp-port 6969

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only files, and runs it through the
exact same bootstrap. With no --enabled-providers every files provider this
binary ships is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := filesProviders()
		opts := newProviderOptionBuilder(cmd)
		opts.set(ftp.ProviderName, "ftp-port", "port", filesFTPFlags.port)
		opts.set(ftp.ProviderName, "ftp-passive-host", "passive_host", filesFTPFlags.passiveHost)
		opts.set(ftp.ProviderName, "ftp-passive-ports", "passive_ports", filesFTPFlags.passivePorts)
		opts.set(ftp.ProviderName, "ftp-username", "username", filesFTPFlags.username)
		opts.set(ftp.ProviderName, "ftp-password", "password", filesFTPFlags.password)
		opts.set(sftp.ProviderName, "sftp-port", "port", filesSFTPFlags.port)
		opts.set(sftp.ProviderName, "sftp-host-key", "host_key_path", filesSFTPFlags.hostKeyPath)
		opts.set(sftp.ProviderName, "sftp-authorized-keys", "authorized_keys", filesSFTPFlags.authorizedKeys)
		opts.set(sftp.ProviderName, "sftp-username", "username", filesSFTPFlags.username)
		opts.set(sftp.ProviderName, "sftp-password", "password", filesSFTPFlags.password)
		opts.set(tftp.ProviderName, "tftp-port", "port", filesTFTPFlags.port)
		return runSinglePlugin(cmd, files.PluginName, func() plugin.Plugin {
			return files.New(ftp.New(), sftp.New(), tftp.New())
		}, providerNames(providers), filesFlags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(filesCmd, &filesFlags)
	registerFTPOptionFlags(filesCmd, &filesFTPFlags)
	registerSFTPOptionFlags(filesCmd, &filesSFTPFlags)
	registerTFTPOptionFlags(filesCmd, &filesTFTPFlags)
	rootCmd.AddCommand(filesCmd)
}
