package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"time"
)

// handshakeTimeout bounds a client's TLS handshake. A client set up for plain
// text never sends one: it waits for the greeting while the server waits for
// its ClientHello, and without a bound both would wait for ever.
const handshakeTimeout = 10 * time.Second

// tlsListener is tls.NewListener with each handshake bounded by
// handshakeTimeout and its failure logged, naming the likely cause. Accept
// returns the *tls.Conn itself, which go-imap and go-smtp look for to know
// the connection is TLS.
type tlsListener struct {
	net.Listener
	config *tls.Config
	proto  string // "IMAP" or "SMTP", for the log
	log    *log.Logger
	// ctx ends with the listener, ending the handshakes still running.
	ctx    context.Context
	cancel context.CancelFunc
}

func newTLSListener(l net.Listener, config *tls.Config, proto string, logger *log.Logger) *tlsListener {
	ctx, cancel := context.WithCancel(context.Background())
	return &tlsListener{Listener: l, config: config, proto: proto, log: logger, ctx: ctx, cancel: cancel}
}

func (l *tlsListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := tls.Server(c, l.config)
	// The server's first read would start the handshake too, without a
	// bound; whichever starts it, HandshakeContext closes the connection
	// when the time is up, failing the server's read. The goroutine ends
	// with the handshake, within handshakeTimeout.
	go l.handshake(tc)
	return tc, nil
}

func (l *tlsListener) handshake(tc *tls.Conn) {
	ctx, cancel := context.WithTimeout(l.ctx, handshakeTimeout)
	defer cancel()
	err := tc.HandshakeContext(ctx)
	if err == nil {
		return
	}
	tc.Close()
	from := tc.RemoteAddr()
	if l.ctx.Err() != nil {
		return // shutting down
	}
	switch _, plain := errors.AsType[tls.RecordHeaderError](err); {
	case errors.Is(err, context.DeadlineExceeded):
		l.log.Printf("%v client %v sent no TLS handshake within %v: set up for plain text? use %v, or run serve with -plaintext",
			l.proto, from, handshakeTimeout, secureScheme(l.proto))
	case plain:
		l.log.Printf("%v client %v sent plain text to a TLS port: use %v, or run serve with -plaintext",
			l.proto, from, secureScheme(l.proto))
	default:
		// A client refusing the certificate ends up here, as a remote
		// "bad certificate" or "unknown certificate authority" alert.
		l.log.Printf("%v TLS handshake with %v failed: %v (does the client trust peroxide's certificate? see 'peroxide status')",
			l.proto, from, err)
	}
}

func (l *tlsListener) Close() error {
	l.cancel()
	return l.Listener.Close()
}

func secureScheme(proto string) string {
	if proto == "IMAP" {
		return "imaps:// (SSL/TLS, not STARTTLS)"
	}
	return "smtps:// (SSL/TLS, not STARTTLS)"
}
