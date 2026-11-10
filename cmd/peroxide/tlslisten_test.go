package main

import (
	"crypto/tls"
	"crypto/x509"
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
// text is cut off at once. Each is logged with the fix. A TLS client gets
// through, and nothing is logged.
// evidence: witnesses
func TestTLSListenerHandshake(t *testing.T) {
	cert := selfSigned(t)
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)

	for _, tc := range []struct {
		name    string
		client  func(t *testing.T, c net.Conn)
		wantLog string        // "" for none
		wantAt  time.Duration // when the server's side is closed
	}{
		{"silent", func(*testing.T, net.Conn) {}, "sent no TLS handshake", handshakeTimeout},
		{"plain text", func(_ *testing.T, c net.Conn) {
			// A pipe is synchronous: read what the server answers.
			go io.Copy(io.Discard, c)
			io.WriteString(c, "a1 LOGIN bob pw\r\n")
		}, "sent plain text to a TLS port", 0},
		{"tls", func(t *testing.T, c net.Conn) {
			tc := tls.Client(c, &tls.Config{RootCAs: pool, ServerName: "localhost"})
			if err := tc.Handshake(); err != nil {
				t.Errorf("client handshake = %v", err)
			}
		}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logged lockedBuffer
				pl := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
				l := newTLSListener(pl, config, "IMAP", log.New(&logged, "", 0))
				defer l.Close()

				client, server := net.Pipe()
				defer client.Close()
				go func() { pl.conns <- server }()
				conn, err := l.Accept()
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()

				start := time.Now()
				tc.client(t, client)
				synctest.Wait()
				time.Sleep(tc.wantAt)
				synctest.Wait()
				got := logged.String()

				if tc.wantLog == "" {
					// Long past the bound: a finished handshake isn't cut off.
					time.Sleep(2 * handshakeTimeout)
					synctest.Wait()
					if got := logged.String(); got != "" {
						t.Errorf("logged %q for a TLS client", got)
					}
					return
				}
				if !strings.Contains(got, tc.wantLog) || !strings.Contains(got, "imaps://") || !strings.Contains(got, "-plaintext") {
					t.Errorf("logged %q, want %q with the fix", got, tc.wantLog)
				}
				if d := time.Since(start); d != tc.wantAt {
					t.Errorf("cut off after %v, want %v", d, tc.wantAt)
				}
				if _, err := conn.Read(make([]byte, 1)); err == nil {
					t.Error("the server's side still reads")
				}
			})
		})
	}
}
