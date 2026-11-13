package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"runtime"
	rtdebug "runtime/debug"
	"syscall"
	"time"

	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-smtp"

	"github.com/seamanticus/peroxide/internal/auth"
	"github.com/seamanticus/peroxide/internal/events"
	imapbackend "github.com/seamanticus/peroxide/internal/imap"
	"github.com/seamanticus/peroxide/internal/login"
	"github.com/seamanticus/peroxide/internal/paths"
	"github.com/seamanticus/peroxide/internal/protonmail"
	smtpbackend "github.com/seamanticus/peroxide/internal/smtp"
	"github.com/seamanticus/peroxide/internal/tlscert"
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
		HTTPClient: &http.Client{Jar: jar, Transport: protonTransport()},
	}
}

// protonTransport bounds the wait for Proton as proton-bridge does
// (internal/dialer/dialer_basic.go): a request whose answer doesn't start
// within 30 s fails, instead of holding the account's login and token refresh,
// which every other request of the account waits for. Bodies have no bound:
// attachments stream for as long as they take.
func protonTransport() *http.Transport {
	// http.DefaultTransport's settings, with the bounds added.
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func askBridgePass() (string, error) {
	if v := os.Getenv("PEROXIDE_BRIDGE_PASS"); v != "" {
		return v, nil
	}
	// Prompts go where logs go: stderr, or stdout with -debug.
	b, err := login.Password(log.Writer(), "Bridge password")
	return string(b), err
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

// smtpIdleTimeout bounds the wait for an SMTP client's next line, and for it
// to take an answer.
const smtpIdleTimeout = 10 * time.Minute

func listenAndServeSMTP(addr string, debug bool, authManager *auth.Manager, tlsConfig *tls.Config) error {
	be := smtpbackend.New(authManager)
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = "localhost" // TODO: make this configurable
	// go-smtp's default writes to os.Stderr, which -debug doesn't capture.
	s.ErrorLog = log.New(log.Writer(), "smtp/server ", log.LstdFlags)
	s.AllowInsecureAuth = tlsConfig == nil
	s.TLSConfig = tlsConfigFor(tlsConfig, "smtp")
	// A client silent this long, between commands, is closed: an open
	// connection holds a file descriptor and a goroutine of serve's. RFC 5321
	// section 4.5.3.2.7 asks a server to wait at least 5 minutes.
	s.ReadTimeout = smtpIdleTimeout
	s.WriteTimeout = smtpIdleTimeout
	if debug {
		return serveSMTPDebug(s, log.Writer())
	}

	if s.TLSConfig != nil {
		l, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return fmt.Errorf("listen for SMTP: %w", err)
		}
		log.Println("SMTP server listening with TLS on", s.Addr)
		return s.Serve(newTLSListener(l, s.TLSConfig, "SMTP", log.Default()))
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
	// A client silent this long is logged out: an open connection holds a
	// file descriptor and a goroutine of serve's. go-imap moves the deadline
	// on each command and each response, and IDLE sends none while nothing
	// changes: RFC 2177 has clients restart IDLE at least every 29 minutes
	// (go-imap's client, aerc's, every 25), so the RFC 3501 minimum holds.
	s.AutoLogout = imapserver.MinAutoLogout
	if debug {
		// Traces go where API traffic is logged, so one stream holds the whole
		// session in order.
		s.Enable(imapDebugExtension{w: log.Writer()})
	}

	if s.TLSConfig != nil {
		l, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return fmt.Errorf("listen for IMAP: %w", err)
		}
		log.Println("IMAP server listening with TLS on", s.Addr)
		return s.Serve(newTLSListener(l, s.TLSConfig, "IMAP", log.Default()))
	}

	log.Println("IMAP server listening on", s.Addr)
	return s.ListenAndServe()
}

// protectMemory makes the process non-dumpable (prctl(2) PR_SET_DUMPABLE):
// it leaves no core dump, and no process of the same user, only root, can
// attach to it or read its memory (ptrace(2), /proc/<pid>/mem), which holds
// the unlocked keys. Programs it starts (the browser of `auth`) are not
// affected: execve(2) sets the flag again.
func protectMemory() error {
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", errno)
	}
	return nil
}

func main() {
	// Before anything secret is read. Not fatal: a sandbox may refuse
	// prctl, and peroxide is then as it was without it.
	if err := protectMemory(); err != nil {
		fmt.Fprintln(os.Stderr, "peroxide: warning: cannot keep other processes from reading this one's memory:", err)
	}
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
		username := inv.args[0]

		if opts.remove {
			if err := auth.DeleteUsername(username); err != nil {
				log.Fatal(err)
			}
			break
		}

		mode, err := login.ParseMode(opts.captchaMode)
		if err != nil {
			log.Fatal(err)
		}

		// Ctrl+C during the wait for Enter ends it cleanly.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Prompts go where logs go: stderr, or stdout with -debug.
		err = login.Run(ctx, newClient(), username, login.Options{
			Mode:    mode,
			Timeout: opts.captchaTimeout,
			Output:  log.Writer(),
		})
		if errors.Is(err, login.ErrKept) {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
	case "list":
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

		certPath, err := tlscert.CertPath()
		if err != nil {
			log.Fatal(err)
		}
		switch fp, err := tlscert.CertFileFingerprint(certPath); {
		case errors.Is(err, os.ErrNotExist):
			fmt.Printf("peroxide's TLS certificate: none yet, the first serve without -tls-cert makes %v\n", certPath)
		case err != nil:
			log.Fatal(err)
		default:
			fmt.Printf("peroxide's TLS certificate (serve's unless given -tls-cert): %v\n  SHA-256 %v\n", certPath, fp)
		}
	case "serve":
		// One process serves both protocols with one auth.Manager: Proton's
		// refresh tokens are single use, so two processes refreshing the
		// same account's session lock one of them out (TODO 18). The lock
		// tells the other commands so, and keeps a second serve out.
		serveLock, err := paths.LockServe()
		if err != nil {
			log.Fatal(err)
		}
		so := inv.serve
		var tlsConfig *tls.Config
		if so.plaintext {
			log.Println("warning: -plaintext: IMAP and SMTP without TLS, the bridge password in clear on loopback")
		} else {
			var created bool
			if tlsConfig, created, err = tlscert.TLS(so.tlsCert, so.tlsKey, so.tlsClientCA); err != nil {
				log.Fatal(err)
			}
			if created {
				certPath, err := tlscert.CertPath()
				if err != nil {
					log.Fatal(err)
				}
				log.Printf("Made peroxide's TLS certificate %v, SHA-256 fingerprint %v: have your clients trust it (docs/commands.md)",
					certPath, tlscert.Fingerprint(tlsConfig.Certificates[0].Certificate[0]))
			}
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
		if running, err := paths.ServeRunning(); err != nil {
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
		if running, err := paths.ServeRunning(); err != nil {
			log.Fatal(err)
		} else if running {
			addr := inv.sendmail.smtpHost + ":" + inv.sendmail.smtpPort
			certPath := inv.sendmail.tlsCert
			if certPath == "" && !inv.sendmail.plaintext {
				if certPath, err = tlscert.CertPath(); err != nil {
					log.Fatal(err)
				}
			}
			if err := sendThroughServe(addr, certPath, username, bridgePassword, rcpt, msg); err != nil {
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
