package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	rtdebug "runtime/debug"
	"strings"
	"syscall"
	"time"

	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-smtp"
	"golang.org/x/term"

	"github.com/seamanticus/peroxide/auth"
	"github.com/seamanticus/peroxide/captcha"
	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/events"
	imapbackend "github.com/seamanticus/peroxide/imap"
	"github.com/seamanticus/peroxide/protonmail"
	"github.com/seamanticus/peroxide/sendrecord"
	smtpbackend "github.com/seamanticus/peroxide/smtp"
)

const (
	defaultAPIEndpoint = "https://mail.proton.me/api"
	defaultAppVersion  = "Other"
)

var (
	debug       bool
	apiEndpoint string
	appVersion  string
)

func newClient() *protonmail.Client {
	// Proton's API hands out a session cookie on /auth/info and expects it
	// back on the requests that follow. Without a jar every request looks like
	// a brand new, sessionless client, which the API treats with suspicion.
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatalf("failed to create cookie jar: %v", err)
	}

	return &protonmail.Client{
		RootURL:    apiEndpoint,
		AppVersion: appVersion,
		Debug:      debug,
		HTTPClient: &http.Client{Jar: jar},
	}
}

func askPass(prompt string) ([]byte, error) {
	f := os.Stdin
	if !term.IsTerminal(int(f.Fd())) {
		// This can happen if stdin is used for piping data
		// TODO: the following assumes Unix
		var err error
		if f, err = os.Open("/dev/tty"); err != nil {
			return nil, err
		}
		defer f.Close()
	}
	// Prompts go where logs go: stderr, or stdout with -debug. Only the
	// prompt is written; the password is read without echo.
	fmt.Fprintf(log.Writer(), "%v: ", prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	if err == nil {
		fmt.Fprintf(log.Writer(), "\n")
	}
	return b, err
}

func askBridgePass() (string, error) {
	if v := os.Getenv("PEROXIDE_BRIDGE_PASS"); v != "" {
		return v, nil
	}
	b, err := askPass("Bridge password")
	return string(b), err
}

// promptConfirm asks a yes/no question where logs go and reports whether
// the answer is y or Y. Everything else, including EOF, means no.
func promptConfirm(prompt string) bool {
	fmt.Fprintf(log.Writer(), "%v", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(scanner.Text())
	return answer == "y" || answer == "Y"
}

// tlsConfigFor returns a copy of the TLS configuration advertising the ALPN
// protocol of a given service (RFC 8314).
//
// The copy matters: the servers would otherwise share one config, and the HTTP
// server sets NextProtos on it for HTTP/2. Clients asking for "imap" or "smtp"
// then find no protocol in common and every connection is dropped.
func tlsConfigFor(tlsConfig *tls.Config, proto string) *tls.Config {
	if tlsConfig == nil {
		return nil
	}
	c := tlsConfig.Clone()
	c.NextProtos = []string{proto}
	return c
}

func listenAndServeSMTP(addr string, debug bool, authManager *auth.Manager, sends *sendrecord.Recorder, tlsConfig *tls.Config) error {
	be := smtpbackend.New(authManager, sends)
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = "localhost" // TODO: make this configurable
	// go-smtp's default writes to os.Stderr, which -debug doesn't capture.
	s.ErrorLog = log.New(log.Writer(), "smtp/server ", log.LstdFlags)
	s.AllowInsecureAuth = tlsConfig == nil
	s.TLSConfig = tlsConfigFor(tlsConfig, "smtp")
	if debug {
		return serveSMTPDebug(s, log.Writer())
	}

	if s.TLSConfig != nil {
		log.Println("SMTP server listening with TLS on", s.Addr)
		return s.ListenAndServeTLS()
	}

	log.Println("SMTP server listening on", s.Addr)
	return s.ListenAndServe()
}

func listenAndServeIMAP(addr string, debug bool, authManager *auth.Manager, eventsManager *events.Manager, sends *sendrecord.Recorder, tlsConfig *tls.Config, showAllMail bool) error {
	be := imapbackend.New(authManager, eventsManager, sends, showAllMail)
	s := imapserver.New(be)
	s.Enable(imapbackend.Extension{})
	// A larger literal is refused before it is read: the client gets BAD, as
	// go-imap gives no way to answer NO [TOOBIG] at that point.
	s.MaxLiteralSize = imapbackend.MaxLiteralSize
	s.Addr = addr
	// go-imap's default writes to os.Stderr, which -debug doesn't capture.
	s.ErrorLog = log.New(log.Writer(), "imap/server: ", log.LstdFlags)
	s.AllowInsecureAuth = tlsConfig == nil
	s.TLSConfig = tlsConfigFor(tlsConfig, "imap")
	if debug {
		// Traces go where API traffic is logged, so one stream holds the whole
		// session in order.
		s.Enable(imapDebugExtension{w: log.Writer()})
	}

	if s.TLSConfig != nil {
		log.Println("IMAP server listening with TLS on", s.Addr)
		return s.ListenAndServeTLS()
	}

	log.Println("IMAP server listening on", s.Addr)
	return s.ListenAndServe()
}

const usage = `usage: peroxide [options...] <command>
Commands:

	auth [options...] <username>			Login to ProtonMail via peroxide

	sendmail <username> -- <args...>		sendmail(1) interface

	download-mailbox [-discard-local-copy] <username>
						Download the account's mail into peroxide's local copy
						now, as serve does in the background after a login, and
						exit once it is complete. Run it before the first client
						connects to a large account; interrupted, it resumes.
						Not while a client of the account is logged in to serve.
						-discard-local-copy wipes the local copy first and
						downloads it all again: every mailbox gets a new
						UIDVALIDITY, so clients refetch.

	serve									Run all servers

	smtp									Run peroxide as an SMTP server

	imap						   			Run peroxide as an IMAP server

	status									View peroxide status

Global options:

	-debug
		Log API requests and responses as received, and IMAP and SMTP
		traffic, with credentials scrubbed. Everything else written to
		stderr, prompts and crashes included, goes to stdout too
	
	-api-endpoint <url>
		ProtonMail API endpoint
	
	-app-version <version>
		ProtonMail application version
	
	-smtp-host example.com
		Allowed SMTP email hostname on which peroxide listens, defaults to 127.0.0.1
	
	-imap-host example.com
		Allowed IMAP email hostname on which peroxide listens, defaults to 127.0.0.1
	
	-smtp-port example.com
		SMTP port on which peroxide listens, defaults to 1025

	-imap-port example.com
		IMAP port on which peroxide listens, defaults to 1143
	
	-disable-imap
		Disable IMAP for peroxide serve
	
	-show-all-mail=false
		Leave the All Mail mailbox out of IMAP LIST, defaults to true
	
	-disable-smtp
		Disable SMTP for peroxide serve
	
	-tls-cert /path/to/cert.pem
		Path to the certificate to use for incoming connections (Optional)
	
	-tls-key /path/to/key.pem
		Path to the certificate key to use for incoming connections (Optional)
	
	-tls-client-ca /path/to/ca.pem
		If set, clients must provide a certificate signed by the given CA (Optional)

Options for "auth" command:
	
	-remove
		Delete the stored credentials for the username instead of logging in
	
	-captcha-mode browser|manual|disabled
		How to complete Proton's human verification challenge when it is
		requested, defaults to browser
	
	-captcha-listen 127.0.0.1:8765
		Address of the local, loopback-only CAPTCHA helper server
	
	-captcha-timeout 10m
		Maximum time to wait for the challenge to be completed
	
	-no-open-browser
		Print the CAPTCHA URL instead of trying to launch a browser
	
	-captcha-endpoint https://mail-api.proton.me
		API host serving the CAPTCHA challenge. It must serve both
		/core/v4/captcha and the widget's assets under /captcha/v1/assets/

Environment variables:
	PEROXIDE_BRIDGE_PASS	Don't prompt for the bridge password, use this variable instead

`

func main() {
	flag.BoolVar(&debug, "debug", false, "Log API, IMAP and SMTP traffic to stdout, credentials scrubbed")
	flag.StringVar(&apiEndpoint, "api-endpoint", defaultAPIEndpoint, "ProtonMail API endpoint")
	flag.StringVar(&appVersion, "app-version", defaultAppVersion, "ProtonMail app version")

	smtpHost := flag.String("smtp-host", "127.0.0.1", "Allowed SMTP email hostname on which peroxide listens, defaults to 127.0.0.1")
	smtpPort := flag.String("smtp-port", "1025", "SMTP port on which peroxide listens, defaults to 1025")
	disableSMTP := flag.Bool("disable-smtp", false, "Disable SMTP for peroxide serve")

	imapHost := flag.String("imap-host", "127.0.0.1", "Allowed IMAP email hostname on which peroxide listens, defaults to 127.0.0.1")
	imapPort := flag.String("imap-port", "1143", "IMAP port on which peroxide listens, defaults to 1143")
	disableIMAP := flag.Bool("disable-imap", false, "Disable IMAP for peroxide serve")
	showAllMail := flag.Bool("show-all-mail", true, "Show the All Mail mailbox in IMAP LIST")

	tlsCert := flag.String("tls-cert", "", "Path to the certificate to use for incoming connections")
	tlsCertKey := flag.String("tls-key", "", "Path to the certificate key to use for incoming connections")
	tlsClientCA := flag.String("tls-client-ca", "", "If set, clients must provide a certificate signed by the given CA")

	authCmd := flag.NewFlagSet("auth", flag.ExitOnError)
	sendmailCmd := flag.NewFlagSet("sendmail", flag.ExitOnError)
	downloadCmd := flag.NewFlagSet("download-mailbox", flag.ExitOnError)

	flag.Usage = func() {
		fmt.Print(usage)
	}

	flag.Parse()

	tlsConfig, err := config.TLS(*tlsCert, *tlsCertKey, *tlsClientCA)
	if err != nil {
		log.Fatal(err)
	}

	if debug {
		// One redirect then captures the whole session in order, as
		// 2>&1 would: prompts and server error logs follow log.Writer(),
		// and a crash, which the runtime writes to fd 2, is copied here.
		log.SetOutput(os.Stdout)
		if err := rtdebug.SetCrashOutput(os.Stdout, rtdebug.CrashOptions{}); err != nil {
			log.Fatalf("copy crashes to stdout: %v", err)
		}
	}
	cmd := flag.Arg(0)
	switch cmd {
	case "auth":
		var captchaMode, captchaListen string
		var captchaTimeout time.Duration
		var noOpenBrowser, remove bool
		authCmd.StringVar(&captchaMode, "captcha-mode", string(captcha.ModeBrowser), "CAPTCHA behavior: browser, manual or disabled")
		authCmd.StringVar(&captchaListen, "captcha-listen", captcha.DefaultListenAddr, "Local CAPTCHA callback address")
		authCmd.DurationVar(&captchaTimeout, "captcha-timeout", captcha.DefaultTimeout, "Maximum time to wait for the CAPTCHA to be completed")
		authCmd.BoolVar(&noOpenBrowser, "no-open-browser", false, "Print the CAPTCHA URL without trying to launch a browser")
		captchaEndpoint := authCmd.String("captcha-endpoint", protonmail.CaptchaEndpoint, "API host serving the CAPTCHA challenge")
		authCmd.BoolVar(&remove, "remove", false, "Delete the stored credentials for the username instead of logging in")
		authCmd.Parse(flag.Args()[1:])
		protonmail.CaptchaEndpoint = *captchaEndpoint
		username := authCmd.Arg(0)
		if username == "" {
			log.Fatal("usage: peroxide auth [options...] <username>")
		}

		if remove {
			if err := auth.DeleteUsername(username); err != nil {
				log.Fatal(err)
			}
			break
		}

		storedUsernames, err := auth.ListUsernames()
		if err != nil {
			log.Fatal(err)
		}
		for _, u := range storedUsernames {
			if u != username {
				continue
			}

			if !promptConfirm(fmt.Sprintf("Credentials for %v already exist, delete them to create new ones? [y/N] ", username)) {
				os.Exit(0)
			}
			if err := auth.DeleteUsername(username); err != nil {
				log.Fatal(err)
			}
			break
		}

		mode, err := captcha.ParseMode(captchaMode)
		if err != nil {
			log.Fatal(err)
		}
		captchaOpts := captcha.Options{
			Mode:        mode,
			ListenAddr:  captchaListen,
			Timeout:     captchaTimeout,
			OpenBrowser: !noOpenBrowser,
			Output:      log.Writer(),
		}

		// Make sure Ctrl+C tears down the CAPTCHA server cleanly.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		c := newClient()

		var a *protonmail.Auth
		/*if cachedAuth, ok := auths[username]; ok {
			var err error
			a, err = c.AuthRefresh(a)
			if err != nil {
				// TODO: handle expired token error
				log.Fatal(err)
			}
		}*/

		var loginPassword string
		if a == nil {
			if pass, err := askPass("Password"); err != nil {
				log.Fatal(err)
			} else {
				loginPassword = string(pass)
			}

			a, err = captcha.Authenticate(ctx, c, username, loginPassword, captchaOpts)
			if err != nil {
				log.Fatal(err)
			}

			if a.TwoFactor.Enabled != 0 {
				if a.TwoFactor.TOTP != 1 {
					log.Fatal("Only TOTP is supported as a 2FA method")
				}

				scanner := bufio.NewScanner(os.Stdin)
				// The code is typed in and echoed by the terminal, never logged.
				fmt.Fprintf(log.Writer(), "2FA TOTP code: ")
				scanner.Scan()
				code := scanner.Text()

				scope, err := c.AuthTOTP(code)
				if err != nil {
					log.Fatal(err)
				}
				a.Scope = scope
			}
		}

		var mailboxPassword string
		if a.PasswordMode == protonmail.PasswordSingle {
			mailboxPassword = loginPassword
		}
		if mailboxPassword == "" {
			prompt := "Password"
			if a.PasswordMode == protonmail.PasswordTwo {
				prompt = "Mailbox password"
			}
			if pass, err := askPass(prompt); err != nil {
				log.Fatal(err)
			} else {
				mailboxPassword = string(pass)
			}
		}

		keySalts, err := c.ListKeySalts()
		if err != nil {
			log.Fatal(err)
		}

		_, err = c.Unlock(a, keySalts, mailboxPassword)
		if err != nil {
			log.Fatal(err)
		}

		secretKey, bridgePassword, err := auth.GeneratePassword()
		if err != nil {
			log.Fatal(err)
		}

		err = auth.EncryptAndSave(&auth.CachedAuth{
			Auth:            *a,
			LoginPassword:   loginPassword,
			MailboxPassword: mailboxPassword,
			KeySalts:        keySalts,
		}, username, secretKey)
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("Bridge password:", bridgePassword)
	case "status":
		usernames, err := auth.ListUsernames()
		if err != nil {
			log.Fatal(err)
		}

		if len(usernames) == 0 {
			fmt.Printf("No logged in user.\n")
		} else {
			fmt.Printf("%v logged in user(s):\n", len(usernames))
			for _, u := range usernames {
				fmt.Printf("- %v\n", u)
			}
		}
	case "smtp":
		addr := *smtpHost + ":" + *smtpPort
		authManager := auth.NewManager(newClient)
		log.Fatal(listenAndServeSMTP(addr, debug, authManager, sendrecord.New(), tlsConfig))
	case "imap":
		addr := *imapHost + ":" + *imapPort
		authManager := auth.NewManager(newClient)
		eventsManager := events.NewManager()
		log.Fatal(listenAndServeIMAP(addr, debug, authManager, eventsManager, sendrecord.New(), tlsConfig, *showAllMail))
	case "serve":
		smtpAddr := *smtpHost + ":" + *smtpPort
		imapAddr := *imapHost + ":" + *imapPort

		authManager := auth.NewManager(newClient)
		eventsManager := events.NewManager()
		// What SMTP sends, APPEND into Sent finds there instead of importing
		// it again: only within one process.
		sends := sendrecord.New()

		done := make(chan error, 2)
		if !*disableSMTP {
			go func() {
				done <- listenAndServeSMTP(smtpAddr, debug, authManager, sends, tlsConfig)
			}()
		}
		if !*disableIMAP {
			go func() {
				done <- listenAndServeIMAP(imapAddr, debug, authManager, eventsManager, sends, tlsConfig, *showAllMail)
			}()
		}
		log.Fatal(<-done)
	case "download-mailbox":
		discard := downloadCmd.Bool("discard-local-copy", false, "wipe the local copy first and download it again from scratch")
		downloadCmd.Parse(flag.Args()[1:])
		username := downloadCmd.Arg(0)
		if username == "" || downloadCmd.NArg() != 1 {
			log.Fatal("usage: peroxide download-mailbox [-discard-local-copy] <username>")
		}

		bridgePassword, err := askBridgePass()
		if err != nil {
			log.Fatal(err)
		}

		// The keys decrypt the bodies, which are stored built, as served.
		c, privateKeys, storeKey, err := auth.NewManager(newClient).Auth(username, bridgePassword)
		if err != nil {
			log.Fatal(err)
		}
		if err := imapbackend.DownloadAccount(c, privateKeys, storeKey, *discard); err != nil {
			log.Fatal(err)
		}
	case "sendmail":
		username := flag.Arg(1)
		if username == "" || flag.Arg(2) != "--" {
			log.Fatal("usage: peroxide sendmail <username> -- <args...>")
		}

		// git send-email always passes -i ("a line holding only . doesn't end
		// the input"), and the FlagSet exits on a flag it doesn't define. stdin
		// is read to its end anyway, so -i is accepted and changes nothing.
		sendmailCmd.Bool("i", false, "accepted for sendmail(1) compatibility; stdin is always read to its end")
		sendmailCmd.Parse(flag.Args()[3:])
		rcpt := sendmailCmd.Args()

		bridgePassword, err := askBridgePass()
		if err != nil {
			log.Fatal(err)
		}

		c, privateKeys, _, err := auth.NewManager(newClient).Auth(username, bridgePassword)
		if err != nil {
			log.Fatal(err)
		}

		u, err := c.GetCurrentUser()
		if err != nil {
			log.Fatal(err)
		}

		addrs, err := c.ListAddresses()
		if err != nil {
			log.Fatal(err)
		}

		if _, err := smtpbackend.SendMail(c, u, privateKeys, addrs, rcpt, os.Stdin); err != nil {
			log.Fatal(err)
		}
	default:
		fmt.Print(usage)
		if cmd != "help" {
			log.Fatal("Unrecognized command")
		}
	}
}
