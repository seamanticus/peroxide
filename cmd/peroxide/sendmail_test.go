package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// relay is an SMTP backend recording what one session was given.
type relay struct {
	mu                 sync.Mutex
	user, pass, from   string
	rcpt               []string
	data               []byte
	wantUser, wantPass string
}

func (r *relay) NewSession(*smtp.Conn) (smtp.Session, error) { return &relaySession{r}, nil }

type relaySession struct{ r *relay }

func (s *relaySession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *relaySession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		s.r.mu.Lock()
		defer s.r.mu.Unlock()
		s.r.user, s.r.pass = user, pass
		if user != s.r.wantUser || pass != s.r.wantPass {
			return errors.New("bad login")
		}
		return nil
	}), nil
}
func (s *relaySession) Mail(from string, _ *smtp.MailOptions) error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.from = from
	return nil
}
func (s *relaySession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.rcpt = append(s.r.rcpt, to)
	return nil
}
func (s *relaySession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.data = b
	return err
}
func (s *relaySession) Reset()        {}
func (s *relaySession) Logout() error { return nil }

// startRelay serves r on a loopback port, with TLS if tlsConfig isn't nil.
func startRelay(t *testing.T, r *relay, tlsConfig *tls.Config) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig != nil {
		l = tls.NewListener(l, tlsConfig)
	}
	s := smtp.NewServer(r)
	s.AllowInsecureAuth = true
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	return l.Addr().String()
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"peroxide.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}
}

const relayMessage = "From: Bob <bob@example.org>\r\nTo: carol@example.org\r\nSubject: hi\r\n\r\nhello\r\n"

// sendmail through serve logs in with the username and bridge password,
// sends from the message's From to the recipients given, the message whole,
// in clear or, when serve has TLS, over TLS trusting serve's certificate only
// (a self-signed one for another name than the address dialled).
// evidence: witnesses
func TestSendThroughServe(t *testing.T) {
	cert := selfSigned(t)
	other := selfSigned(t)
	for _, tc := range []struct {
		name          string
		serverCert    *tls.Certificate // nil: no TLS
		pinnedCert    *tls.Certificate // serve's, as sendmail reads it
		password      string
		wantErrIn     string
		wantDelivered bool
	}{
		{"plain", nil, nil, "pw", "", true},
		{"tls pinned", &cert, &cert, "pw", "", true},
		{"tls other certificate", &other, &cert, "pw", "not serve's", false},
		{"wrong password", nil, nil, "nope", "refused the login", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &relay{wantUser: "bob", wantPass: "pw"}
			var serverTLS *tls.Config
			certPath := ""
			if tc.serverCert != nil {
				serverTLS = &tls.Config{Certificates: []tls.Certificate{*tc.serverCert}}
				certPath = filepath.Join(t.TempDir(), "cert.pem")
				pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tc.pinnedCert.Certificate[0]})
				if err := os.WriteFile(certPath, pemBytes, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			addr := startRelay(t, r, serverTLS)
			err := sendThroughServe(addr, certPath, "bob", tc.password, []string{"carol@example.org", "dan@example.org"}, []byte(relayMessage))
			if tc.wantErrIn != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrIn) {
					t.Errorf("sendThroughServe() = %v, want an error with %q", err, tc.wantErrIn)
				}
			} else if err != nil {
				t.Fatalf("sendThroughServe() = %v", err)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			delivered := r.data != nil
			if delivered != tc.wantDelivered {
				t.Fatalf("delivered = %v, want %v", delivered, tc.wantDelivered)
			}
			if !delivered {
				return
			}
			if r.from != "bob@example.org" || !slices.Equal(r.rcpt, []string{"carol@example.org", "dan@example.org"}) {
				t.Errorf("envelope from %q to %q", r.from, r.rcpt)
			}
			if !bytes.Equal(r.data, []byte(relayMessage)) {
				t.Errorf("data = %q, want the message", r.data)
			}
		})
	}
}

// With serve's SMTP not listening (-disable-smtp), sendmail fails, rather
// than log in itself and spend the token serve holds.
// evidence: witnesses
func TestSendThroughServeNotListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	err = sendThroughServe(addr, "", "bob", "pw", []string{"carol@example.org"}, []byte(relayMessage))
	if err == nil || !strings.Contains(err.Error(), "can't be reached") {
		t.Errorf("sendThroughServe() = %v, want it unreachable", err)
	}
}

// A server that never greets, as serve with TLS does to a plain client and
// the other way round, fails sendmail after greetTimeout, not go-smtp's five
// minutes, with a hint at the TLS setting.
// evidence: witnesses
func TestSendThroughServeNoGreeting(t *testing.T) {
	pinned, err := pinnedTLS(writeCertPEM(t, selfSigned(t).Certificate[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		pinned *tls.Config
	}{{"plain", nil}, {"tls", pinned}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, server := net.Pipe()
				defer server.Close()
				// Reads what the client sends (a TLS ClientHello), says nothing.
				go io.Copy(io.Discard, server)
				start := time.Now()
				err := sendOver(client, "serve", tc.pinned, "bob", "pw", []string{"carol@example.org"}, []byte(relayMessage))
				if err == nil || !strings.Contains(err.Error(), "didn't greet") || !strings.Contains(err.Error(), "-plaintext") {
					t.Errorf("sendOver() = %v, want no greeting, with the TLS hint", err)
				}
				if d := time.Since(start); d != greetTimeout {
					t.Errorf("gave up after %v, want %v", d, greetTimeout)
				}
			})
		})
	}
}

// Once DATA is accepted the message is sent: a QUIT that fails (the
// connection dropped) is logged, and sendmail succeeds, so that git
// send-email doesn't send it again.
// evidence: witnesses
func TestSendThroughServeQuitFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			tp := textproto.NewConn(server)
			tp.PrintfLine("220 serve")
			for {
				line, err := tp.ReadLine()
				if err != nil {
					return
				}
				switch verb, _, _ := strings.Cut(line, " "); verb {
				case "EHLO":
					tp.PrintfLine("250-serve\r\n250 AUTH PLAIN")
				case "AUTH":
					tp.PrintfLine("235 ok")
				case "MAIL", "RCPT":
					tp.PrintfLine("250 ok")
				case "DATA":
					tp.PrintfLine("354 go on")
					if _, err := tp.ReadDotBytes(); err != nil {
						return
					}
					tp.PrintfLine("250 queued")
				default: // QUIT: dropped unanswered
					return
				}
			}
		}()
		if err := sendOver(client, "serve", nil, "bob", "pw", []string{"carol@example.org"}, []byte(relayMessage)); err != nil {
			t.Errorf("sendOver() = %v, want nil: the message was accepted", err)
		}
	})
}

// serve's -tls-cert may hold the key too, before the certificate, as
// tls.LoadX509KeyPair allows: the certificate is still the one pinned.
// evidence: witnesses
func TestPinnedTLSKeyBeforeCert(t *testing.T) {
	cert := selfSigned(t)
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "both.pem")
	data := append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})...)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pinned, err := pinnedTLS(p)
	if err != nil {
		t.Fatalf("pinnedTLS() = %v", err)
	}
	if err := pinned.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}); err != nil {
		t.Errorf("serve's certificate refused: %v", err)
	}

	onlyKey := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(onlyKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedTLS(onlyKey); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("pinnedTLS(key only) = %v, want no certificate", err)
	}
}

func writeCertPEM(t *testing.T, der []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
