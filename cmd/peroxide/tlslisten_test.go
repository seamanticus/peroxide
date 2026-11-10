package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A client that never starts the handshake (set up for plain text, waiting
// for a greeting) is cut off after handshakeTimeout; one that sends plain
// text is cut off at once; one that hangs up mid-handshake is said to have.
// Each is logged once and never handed to the server. A TLS client is handed
// on, and nothing is logged.
// evidence: witnesses
func TestTLSListenerHandshake(t *testing.T) {
	cert := selfSigned(t)
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)

	for _, tc := range []struct {
		name    string
		client  func(t *testing.T, c net.Conn)
		wantLog []string      // nil: handed on, nothing logged
		wantAt  time.Duration // when the server's side is closed
	}{
		{"silent", func(*testing.T, net.Conn) {}, []string{"sent no TLS handshake", "imaps://", "-plaintext"}, handshakeTimeout},
		{"plain text", func(_ *testing.T, c net.Conn) {
			// A pipe is synchronous: read what the server answers.
			go io.Copy(io.Discard, c)
			io.WriteString(c, "a1 LOGIN bob pw\r\n")
		}, []string{"sent plain text to a TLS port", "imaps://", "-plaintext"}, 0},
		{"hangs up", func(_ *testing.T, c net.Conn) { c.Close() },
			[]string{"closed the connection during the TLS handshake"}, 0},
		{"tls", func(t *testing.T, c net.Conn) {
			tc := tls.Client(c, &tls.Config{
				RootCAs:    pool,
				ServerName: "localhost",
				// synctest's clock starts in 2000, before the certificate,
				// made outside the bubble: verify within its validity.
				Time: func() time.Time { return cert.Leaf.NotBefore.Add(time.Minute) },
			})
			if err := tc.Handshake(); err != nil {
				t.Errorf("client handshake = %v", err)
			}
		}, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logged lockedBuffer
				pl := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
				l := newTLSListener(pl, config, "IMAP", log.New(&logged, "", 0))

				accepted := make(chan net.Conn, 1)
				acceptErr := make(chan error, 1)
				go func() {
					c, err := l.Accept()
					if err != nil {
						acceptErr <- err
						return
					}
					accepted <- c
				}()

				client, server := net.Pipe()
				defer client.Close()
				pl.conns <- server

				start := time.Now()
				tc.client(t, client)
				synctest.Wait()
				time.Sleep(tc.wantAt)
				synctest.Wait()

				if tc.wantLog == nil {
					select {
					case c := <-accepted:
						if _, ok := c.(*tls.Conn); !ok {
							t.Errorf("Accept() = %T, want *tls.Conn", c)
						}
						c.Close()
					default:
						t.Error("the TLS connection wasn't handed on")
					}
					// Long past the bound: a finished handshake isn't cut off.
					time.Sleep(2 * handshakeTimeout)
					synctest.Wait()
					if got := logged.String(); got != "" {
						t.Errorf("logged %q for a TLS client", got)
					}
				} else {
					got := logged.String()
					for _, want := range tc.wantLog {
						if !strings.Contains(got, want) {
							t.Errorf("logged %q, want %q in it", got, want)
						}
					}
					if n := strings.Count(got, "\n"); n != 1 {
						t.Errorf("logged %v lines, want 1", n)
					}
					if d := time.Since(start); d != tc.wantAt {
						t.Errorf("cut off after %v, want %v", d, tc.wantAt)
					}
					select {
					case c := <-accepted:
						t.Errorf("a failed handshake was handed on: %v", c.RemoteAddr())
					default:
					}
				}

				// Closing ends the accept loop and the waiting Accept, and
				// logs nothing more.
				before := logged.String()
				l.Close()
				synctest.Wait()
				if tc.wantLog != nil {
					if err := <-acceptErr; !errors.Is(err, net.ErrClosed) {
						t.Errorf("Accept() after Close = %v, want net.ErrClosed", err)
					}
				}
				if got := logged.String(); got != before {
					t.Errorf("Close logged %q", strings.TrimPrefix(got, before))
				}
			})
		})
	}
}
