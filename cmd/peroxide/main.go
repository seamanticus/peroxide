package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"runtime"
	rtdebug "runtime/debug"
	"strings"
	"syscall"

	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-smtp"
	"golang.org/x/term"

	"github.com/seamanticus/peroxide/auth"
	"github.com/seamanticus/peroxide/captcha"
	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/events"
	imapbackend "github.com/seamanticus/peroxide/imap"
	"github.com/seamanticus/peroxide/protonmail"
	smtpbackend "github.com/seamanticus/peroxide/smtp"
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

func listenAndServeSMTP(addr string, debug bool, authManager *auth.Manager, tlsConfig *tls.Config) error {
	be := smtpbackend.New(authManager)
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

func listenAndServeIMAP(addr string, debug bool, authManager *auth.Manager, eventsManager *events.Manager, tlsConfig *tls.Config, showAllMail bool) error {
	be := imapbackend.New(authManager, eventsManager, showAllMail)
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

func main() {
	inv, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "peroxide:", err)
		os.Exit(2)
	}
	if inv.global.version {
		info, ok := rtdebug.ReadBuildInfo()
		fmt.Print(versionText(info, ok))
		return
	}
	debug = inv.global.debug
	apiEndpoint = inv.global.apiEndpoint
	appVersion = inv.global.appVersion

	if debug {
		// One redirect then captures the whole session in order, as
		// 2>&1 would: prompts and server error logs follow log.Writer(),
		// and a crash, which the runtime writes to fd 2, is copied here.
		log.SetOutput(os.Stdout)
		if err := rtdebug.SetCrashOutput(os.Stdout, rtdebug.CrashOptions{}); err != nil {
			log.Fatalf("copy crashes to stdout: %v", err)
		}
	}
	switch inv.command {
	case "auth":
		opts := inv.auth
		protonmail.CaptchaEndpoint = opts.captchaEndpoint
		username := inv.args[0]

		if opts.remove {
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

		mode, err := captcha.ParseMode(opts.captchaMode)
		if err != nil {
			log.Fatal(err)
		}
		captchaOpts := captcha.Options{
			Mode:        mode,
			ListenAddr:  opts.captchaListen,
			Timeout:     opts.captchaTimeout,
			OpenBrowser: !opts.noOpenBrowser,
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
	case "serve":
		// One process serves both protocols with one auth.Manager: Proton's
		// refresh tokens are single use, so two processes refreshing the
		// same account's session lock one of them out (TODO 18). The lock
		// tells the other commands so, and keeps a second serve out.
		serveLock, err := config.LockServe()
		if err != nil {
			log.Fatal(err)
		}
		so := inv.serve
		tlsConfig, err := config.TLS(so.tlsCert, so.tlsKey, so.tlsClientCA)
		if err != nil {
			log.Fatal(err)
		}
		smtpAddr := so.smtpHost + ":" + so.smtpPort
		imapAddr := so.imapHost + ":" + so.imapPort

		authManager := auth.NewManager(newClient)
		eventsManager := events.NewManager()

		done := make(chan error, 2)
		if !so.disableSMTP {
			go func() {
				done <- listenAndServeSMTP(smtpAddr, debug, authManager, tlsConfig)
			}()
		}
		if !so.disableIMAP {
			go func() {
				done <- listenAndServeIMAP(imapAddr, debug, authManager, eventsManager, tlsConfig, so.showAllMail)
			}()
		}
		err = <-done
		// Held until the process exits: unreachable, the lock's file would be
		// closed by its finalizer, releasing it while serve runs.
		runtime.KeepAlive(serveLock)
		log.Fatal(err)
	case "download-mailbox":
		username := inv.args[0]
		// Before logging in: that spends the refresh token serve holds.
		if running, err := config.ServeRunning(); err != nil {
			log.Fatal(err)
		} else if running {
			log.Fatal("peroxide serve is running: it downloads the account itself once a client logs in; stop it to run download-mailbox")
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
		if err := imapbackend.DownloadAccount(c, privateKeys, storeKey, inv.discard); err != nil {
			log.Fatal(err)
		}
	case "sendmail":
		username := inv.args[0]
		rcpt := inv.sendmail.recipients

		bridgePassword, err := askBridgePass()
		if err != nil {
			log.Fatal(err)
		}
		msg, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(fmt.Errorf("cannot read the message: %w", err))
		}

		// While serve runs, through its SMTP server: logging in here would
		// spend the refresh token serve holds.
		if running, err := config.ServeRunning(); err != nil {
			log.Fatal(err)
		} else if running {
			addr := inv.sendmail.smtpHost + ":" + inv.sendmail.smtpPort
			if err := sendThroughServe(addr, inv.sendmail.tlsCert, username, bridgePassword, rcpt, msg); err != nil {
				log.Fatal(err)
			}
			return
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

		err = smtpbackend.SendMail(c, u, privateKeys, addrs, rcpt, bytes.NewReader(msg))
		if err != nil {
			log.Fatal(err)
		}
	default:
		panic("invariant: parseArgs returns known commands only")
	}
}
