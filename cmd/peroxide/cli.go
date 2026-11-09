package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	rtdebug "runtime/debug"
	"strings"
	"time"

	"github.com/seamanticus/peroxide/captcha"
	"github.com/seamanticus/peroxide/protonmail"
)

// usage is the whole help: `peroxide help`, `peroxide -h`, `peroxide
// <command> -h`. TestUsageListsEveryOption keeps it in step with the options
// parseArgs defines.
const usage = `peroxide: a Proton Mail bridge for IMAP and SMTP clients

usage: peroxide [-debug] <command> [options] [arguments]
       peroxide -V

Commands:

  auth [options] <username>
      Log in to Proton and print the bridge password, which clients use
      as their password. Asks again if the username is stored already.
        -remove                  delete the stored credentials and exit
        -captcha-mode MODE       browser, manual or disabled (browser)
        -captcha-listen ADDR     address of the CAPTCHA helper (127.0.0.1:8765)
        -captcha-timeout D       how long to wait for the challenge (10m)
        -no-open-browser         print the CAPTCHA URL, don't open it
        -captcha-endpoint URL    host serving the challenge
                                 (https://mail-api.proton.me)

  serve [options]
      Run the IMAP and SMTP servers. One serve at a time.
        -imap-host H, -imap-port P   where IMAP listens (127.0.0.1:1143)
        -smtp-host H, -smtp-port P   where SMTP listens (127.0.0.1:1025)
        -disable-imap                don't start the IMAP server
        -disable-smtp                don't start the SMTP server
        -show-all-mail=false         leave All Mail out of LIST
        -tls-cert F, -tls-key F      serve over TLS with this certificate
        -tls-client-ca F             with TLS, require client certificates
                                     signed by this CA

  sendmail [options] <username> -- <recipient>...
      Send the message read from stdin, as sendmail(1) does, for
      git send-email. While serve runs, sends through it: give it the
      same -smtp-* and -tls-cert options as serve.
        -smtp-host H, -smtp-port P   serve's SMTP (127.0.0.1:1025)
        -tls-cert F                  serve's certificate, trusted alone
        -i                           accepted after --, changes nothing

  download-mailbox [-discard-local-copy] <username>
      Fill the local copy of the account now, then exit; interrupted, it
      resumes. Not while serve runs: serve downloads by itself.
        -discard-local-copy      wipe the local copy and download it all
                                 again (clients refetch every mailbox)

  status
      List the accounts logged in.

  help
      Print this page.

Global options, before or after the command:
  -debug          log API, IMAP and SMTP traffic to stdout, credentials removed
  -V              print the commit this binary was built from, and exit

Options for testing against Proton, any command:
  -api-endpoint URL   Proton's API (https://mail.proton.me/api)
  -app-version V      the x-pm-appversion sent to Proton (Other)

Environment:
  PEROXIDE_BRIDGE_PASS   the bridge password, instead of asking for it

Files: $XDG_CONFIG_HOME/peroxide (credentials, lock),
       $XDG_DATA_HOME/peroxide (local copy of the mail)

Manual: docs/ in the source, https://github.com/seamanticus/peroxide
`

const (
	defaultAPIEndpoint = "https://mail.proton.me/api"
	defaultAppVersion  = "Other"
)

// globalOptions are accepted before the command and after it.
type globalOptions struct {
	debug       bool
	version     bool
	apiEndpoint string
	appVersion  string
}

type authOptions struct {
	remove          bool
	captchaMode     string
	captchaListen   string
	captchaTimeout  time.Duration
	noOpenBrowser   bool
	captchaEndpoint string
}

type serveOptions struct {
	imapHost, imapPort       string
	smtpHost, smtpPort       string
	disableIMAP, disableSMTP bool
	showAllMail              bool
	tlsCert, tlsKey          string
	tlsClientCA              string
}

type sendmailOptions struct {
	smtpHost, smtpPort string
	tlsCert            string
	recipients         []string
}

// invocation is a parsed command line.
type invocation struct {
	global   globalOptions
	command  string
	args     []string // the command's arguments, after its options
	auth     authOptions
	serve    serveOptions
	sendmail sendmailOptions
	discard  bool // download-mailbox -discard-local-copy
}

// errHelp asks for the help page: `help`, or -h anywhere.
var errHelp = errors.New("help asked for")

// newFlagSet makes a flag set that prints nothing: parseArgs words the errors.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func addGlobal(fs *flag.FlagSet, g *globalOptions) {
	fs.BoolVar(&g.debug, "debug", g.debug, "")
	fs.BoolVar(&g.version, "V", g.version, "")
	fs.StringVar(&g.apiEndpoint, "api-endpoint", g.apiEndpoint, "")
	fs.StringVar(&g.appVersion, "app-version", g.appVersion, "")
}

// commandFlags returns the flag set of command, filling inv, and whether
// command is one.
func commandFlags(command string, inv *invocation) (*flag.FlagSet, bool) {
	fs := newFlagSet(command)
	addGlobal(fs, &inv.global)
	switch command {
	case "auth":
		a := &inv.auth
		fs.BoolVar(&a.remove, "remove", false, "")
		fs.StringVar(&a.captchaMode, "captcha-mode", string(captcha.ModeBrowser), "")
		fs.StringVar(&a.captchaListen, "captcha-listen", captcha.DefaultListenAddr, "")
		fs.DurationVar(&a.captchaTimeout, "captcha-timeout", captcha.DefaultTimeout, "")
		fs.BoolVar(&a.noOpenBrowser, "no-open-browser", false, "")
		fs.StringVar(&a.captchaEndpoint, "captcha-endpoint", protonmail.CaptchaEndpoint, "")
	case "serve":
		s := &inv.serve
		fs.StringVar(&s.imapHost, "imap-host", "127.0.0.1", "")
		fs.StringVar(&s.imapPort, "imap-port", "1143", "")
		fs.StringVar(&s.smtpHost, "smtp-host", "127.0.0.1", "")
		fs.StringVar(&s.smtpPort, "smtp-port", "1025", "")
		fs.BoolVar(&s.disableIMAP, "disable-imap", false, "")
		fs.BoolVar(&s.disableSMTP, "disable-smtp", false, "")
		fs.BoolVar(&s.showAllMail, "show-all-mail", true, "")
		fs.StringVar(&s.tlsCert, "tls-cert", "", "")
		fs.StringVar(&s.tlsKey, "tls-key", "", "")
		fs.StringVar(&s.tlsClientCA, "tls-client-ca", "", "")
	case "sendmail":
		s := &inv.sendmail
		fs.StringVar(&s.smtpHost, "smtp-host", "127.0.0.1", "")
		fs.StringVar(&s.smtpPort, "smtp-port", "1025", "")
		fs.StringVar(&s.tlsCert, "tls-cert", "", "")
	case "download-mailbox":
		fs.BoolVar(&inv.discard, "discard-local-copy", false, "")
	case "status", "help":
	default:
		return nil, false
	}
	return fs, true
}

var commands = []string{"auth", "serve", "sendmail", "download-mailbox", "status", "help"}

// undefinedFlag returns the option a flag set's error names as unknown,
// without its leading dash.
func undefinedFlag(err error) (string, bool) {
	name, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: ")
	return strings.TrimLeft(name, "-"), ok
}

// parseArgs parses peroxide's command line, args without the program's name.
// Global options may come before the command or after it; a command's own
// options only after it. Nothing is ignored: an option no command knows, one
// of a command written before it, or arguments a command doesn't take, are
// errors that say what to write instead. -h anywhere, or `help`, returns
// errHelp.
func parseArgs(args []string) (*invocation, error) {
	inv := &invocation{global: globalOptions{apiEndpoint: defaultAPIEndpoint, appVersion: defaultAppVersion}}
	top := newFlagSet("peroxide")
	addGlobal(top, &inv.global)
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		if name, ok := undefinedFlag(err); ok {
			for _, c := range commands {
				if fs, _ := commandFlags(c, &invocation{}); fs.Lookup(name) != nil {
					return nil, fmt.Errorf("-%v is an option of %v: write it after the command\n  peroxide %v -%v", name, c, c, name)
				}
			}
			return nil, fmt.Errorf("unknown option -%v (see 'peroxide help')", name)
		}
		return nil, err
	}
	if inv.global.version {
		return inv, nil
	}
	if top.NArg() == 0 {
		return nil, errHelp
	}

	inv.command = top.Arg(0)
	fs, ok := commandFlags(inv.command, inv)
	if !ok {
		return nil, fmt.Errorf("unknown command %q (see 'peroxide help')", inv.command)
	}
	if err := fs.Parse(top.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		if name, ok := undefinedFlag(err); ok {
			return nil, fmt.Errorf("peroxide %v: unknown option -%v (see 'peroxide help')", inv.command, name)
		}
		return nil, fmt.Errorf("peroxide %v: %w", inv.command, err)
	}
	if inv.global.version {
		return inv, nil
	}
	inv.args = fs.Args()

	wrongArgs := func(want string) error {
		return fmt.Errorf("usage: peroxide %v (see 'peroxide help')", want)
	}
	switch inv.command {
	case "help":
		return nil, errHelp
	case "serve", "status":
		if len(inv.args) != 0 {
			return nil, fmt.Errorf("peroxide %v takes no arguments, got %q (options go before them; see 'peroxide help')", inv.command, inv.args)
		}
	case "auth":
		if len(inv.args) != 1 {
			return nil, wrongArgs("auth [options] <username>")
		}
	case "download-mailbox":
		if len(inv.args) != 1 {
			return nil, wrongArgs("download-mailbox [-discard-local-copy] <username>")
		}
	case "sendmail":
		if len(inv.args) < 2 || inv.args[1] != "--" {
			return nil, wrongArgs("sendmail [options] <username> -- <recipient>...")
		}
		// After --, what git send-email adds: -i, then the recipients.
		// stdin is read to its end anyway, so -i changes nothing.
		rest := newFlagSet("sendmail")
		rest.Bool("i", false, "")
		if err := rest.Parse(inv.args[2:]); err != nil {
			if name, ok := undefinedFlag(err); ok {
				return nil, fmt.Errorf("peroxide sendmail: unknown sendmail(1) option -%v after -- (only -i is understood)", name)
			}
			return nil, fmt.Errorf("peroxide sendmail: %w", err)
		}
		inv.sendmail.recipients = rest.Args()
		inv.args = inv.args[:1]
	}
	return inv, nil
}

// versionText is what -V prints, from the build information Go records in a
// binary built in a git checkout; without it, the module version is all
// there is.
func versionText(info *rtdebug.BuildInfo, ok bool) string {
	var sb strings.Builder
	sb.WriteString("this peroxide binary was compiled from:\n\n")
	if !ok {
		sb.WriteString("commit   : unknown (no build information)\n")
		return sb.String()
	}
	settings := make(map[string]string)
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	revision := settings["vcs.revision"]
	if revision == "" {
		fmt.Fprintf(&sb, "version  : %v (not built in a git checkout: no commit)\n", info.Main.Version)
		fmt.Fprintf(&sb, "go       : %v %v/%v\n", info.GoVersion, settings["GOOS"], settings["GOARCH"])
		return sb.String()
	}
	fmt.Fprintf(&sb, "commit   : %v\n", revision)
	if t, err := time.Parse(time.RFC3339, settings["vcs.time"]); err == nil {
		fmt.Fprintf(&sb, "date     : %v\n", t.UTC().Format("2006-01-02 15:04 UTC"))
	}
	modified := settings["vcs.modified"] == "true"
	if modified {
		sb.WriteString("modified : yes (dirty)\n")
	} else {
		sb.WriteString("modified : no (HEAD)\n")
	}
	fmt.Fprintf(&sb, "go       : %v %v/%v\n", info.GoVersion, settings["GOOS"], settings["GOARCH"])
	fmt.Fprintf(&sb, "url      : <https://github.com/seamanticus/peroxide/commit/%v>\n", revision)
	if modified {
		sb.WriteString("\nwarning: built with uncommitted changes, which the commit above doesn't hold\n")
	}
	return sb.String()
}
