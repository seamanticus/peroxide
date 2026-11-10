package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/mail"
	"os"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// greetTimeout bounds the dial, the TLS handshake and serve's greeting. serve
// greets at once; one waiting longer most likely expects the other of TLS and
// plain text, and would otherwise hold sendmail for go-smtp's five minutes.
const greetTimeout = 10 * time.Second

// sendThroughServe hands a message to the running peroxide serve's SMTP
// server at addr, logged in as username with the bridge password, so that the
// account's Proton session stays the one serve holds: a session of its own
// would spend the refresh token serve has (TODO 18). The envelope sender is
// the message's From; the recipients are rcpt. With certPath, serve's
// -tls-cert, the connection is TLS and that certificate is pinned: only the
// very certificate serve presents is trusted, whatever names it holds.
func sendThroughServe(addr, certPath string, username, password string, rcpt []string, msg []byte) error {
	var pinned *tls.Config
	if certPath != "" {
		var err error
		if pinned, err = pinnedTLS(certPath); err != nil {
			return err
		}
	}
	conn, err := net.DialTimeout("tcp", addr, greetTimeout)
	if err != nil {
		return fmt.Errorf("peroxide serve is running, but its SMTP server at %v can't be reached (run with -disable-smtp?): %w", addr, err)
	}
	return sendOver(conn, addr, pinned, username, password, rcpt, msg)
}

// sendOver is sendThroughServe on the connection conn to addr, TLS when
// pinned isn't nil. It closes conn.
func sendOver(conn net.Conn, addr string, pinned *tls.Config, username, password string, rcpt []string, msg []byte) error {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		conn.Close()
		return fmt.Errorf("cannot read the message: %w", err)
	}
	from, err := mail.ParseAddress(m.Header.Get("From"))
	if err != nil {
		conn.Close()
		return fmt.Errorf("cannot read the message's From: %w", err)
	}

	hint := "is serve using TLS? then don't give sendmail -plaintext"
	if pinned != nil {
		// The handshake runs on the greeting's first read, in its deadline.
		conn = tls.Client(conn, pinned)
		hint = "is serve running with -plaintext, or asking for a client certificate (-tls-client-ca)? give sendmail the same -plaintext, or serve's -tls-cert"
	}
	c := smtp.NewClient(conn)
	defer c.Close()
	commandTimeout := c.CommandTimeout
	c.CommandTimeout = greetTimeout
	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("peroxide serve's SMTP server at %v didn't greet (%v): %w", addr, hint, err)
	}
	c.CommandTimeout = commandTimeout

	if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
		return fmt.Errorf("peroxide serve refused the login: %w", err)
	}
	if err := c.Mail(from.Address, nil); err != nil {
		return err
	}
	for _, to := range rcpt {
		if err := c.Rcpt(to, nil); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	// The message is accepted: failing now would have it sent again.
	if err := c.Quit(); err != nil {
		log.Printf("warning: the message was sent, but QUIT failed: %v", err)
	}
	return nil
}

// pinnedTLS is a client configuration trusting only the certificate in the
// PEM file certPath (serve's own, or its -tls-cert: the first certificate,
// the one serve presents), compared byte for byte: serve's certificate is
// often self-signed, for a name other than the loopback address dialled.
func pinnedTLS(certPath string) (*tls.Config, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read serve's certificate: %w", err)
	}
	// The first certificate, as tls.LoadX509KeyPair takes it: the file may
	// hold the key too, before or after it.
	var want []byte
	for rest := data; want == nil; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return nil, fmt.Errorf("%v holds no PEM certificate", certPath)
		}
		if block.Type == "CERTIFICATE" {
			want = block.Bytes
		}
	}
	if _, err := x509.ParseCertificate(want); err != nil {
		return nil, fmt.Errorf("cannot read serve's certificate: %w", err)
	}
	return &tls.Config{
		// Verification is VerifyConnection's pin, not the system's roots and
		// a host name. VerifyConnection, unlike VerifyPeerCertificate, also
		// runs on a resumed session.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, want) {
				return errors.New("the SMTP server's certificate is not serve's")
			}
			return nil
		},
		// serve's SMTP listener accepts only this protocol (ALPACA).
		NextProtos: []string{"smtp"},
		MinVersion: tls.VersionTLS12,
	}, nil
}
