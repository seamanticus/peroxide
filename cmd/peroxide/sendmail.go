package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/mail"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// sendThroughServe hands a message to the running peroxide serve's SMTP
// server at addr, logged in as username with the bridge password, so that the
// account's Proton session stays the one serve holds: a session of its own
// would spend the refresh token serve has (TODO 18). The envelope sender is
// the message's From; the recipients are rcpt. With tlsConfig, serve's TLS
// configuration from the same flags, the connection is TLS and serve's
// certificate is pinned: only the very certificate serve presents is
// trusted, whatever names it holds.
func sendThroughServe(addr string, tlsConfig *tls.Config, username, password string, rcpt []string, msg []byte) error {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		return fmt.Errorf("cannot read the message: %w", err)
	}
	from, err := mail.ParseAddress(m.Header.Get("From"))
	if err != nil {
		return fmt.Errorf("cannot read the message's From: %w", err)
	}

	var c *smtp.Client
	if tlsConfig == nil {
		c, err = smtp.Dial(addr)
	} else {
		var pinned *tls.Config
		if pinned, err = pinnedTLS(tlsConfig); err == nil {
			c, err = smtp.DialTLS(addr, pinned)
		}
	}
	if err != nil {
		return fmt.Errorf("peroxide serve is running, but its SMTP server at %v can't be reached (run with -disable-smtp?): %w", addr, err)
	}
	defer c.Close()

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
	return c.Quit()
}

// pinnedTLS is a client configuration trusting only the certificate serve's
// configuration presents, compared byte for byte: serve's certificate is
// often self-signed, for a name other than the loopback address dialled.
func pinnedTLS(serve *tls.Config) (*tls.Config, error) {
	if serve.ClientAuth == tls.RequireAndVerifyClientCert {
		return nil, errors.New("serve asks clients for a certificate (-tls-client-ca): sendmail has none")
	}
	if len(serve.Certificates) == 0 || len(serve.Certificates[0].Certificate) == 0 {
		return nil, errors.New("serve's TLS configuration has no certificate")
	}
	want := serve.Certificates[0].Certificate[0]
	if _, err := x509.ParseCertificate(want); err != nil {
		return nil, fmt.Errorf("cannot read serve's certificate: %w", err)
	}
	return &tls.Config{
		// Verification is VerifyPeerCertificate's pin, not the system's
		// roots and a host name.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 || !bytes.Equal(rawCerts[0], want) {
				return errors.New("the SMTP server's certificate is not serve's")
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}, nil
}
