package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// handshakeTimeout bounds a client's TLS handshake. A client set up for plain
// text never sends one: it waits for the greeting while the server waits for
// its ClientHello, and without a bound both would wait for ever.
const handshakeTimeout = 10 * time.Second

// tlsListener is tls.NewListener that hands a connection on only once its
// handshake is done. Each handshake runs in its own goroutine, bounded by
// handshakeTimeout; a failure closes the connection and is logged once,
// naming the likely cause, and the IMAP or SMTP server never sees it, so it
// neither logs it again nor holds a goroutine for it. Accept returns the
// *tls.Conn itself, which go-imap and go-smtp look for to know the
// connection is TLS.
type tlsListener struct {
	net.Listener
	config *tls.Config
	proto  string // "IMAP" or "SMTP", for the log
	log    *log.Logger

	conns chan net.Conn // handshakes done
	errs  chan error    // the underlying Accept's errors
	// ctx ends with Close, ending the accept loop and the handshakes; wg
	// counts them, for Close to wait.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newTLSListener(l net.Listener, config *tls.Config, proto string, logger *log.Logger) *tlsListener {
	ctx, cancel := context.WithCancel(context.Background())
	tl := &tlsListener{
		Listener: l, config: config, proto: proto, log: logger,
		conns: make(chan net.Conn), errs: make(chan error),
		ctx: ctx, cancel: cancel,
	}
	tl.wg.Go(tl.acceptLoop)
	return tl
}

// acceptLoop accepts connections and starts their handshakes, until Close.
// An error goes to Accept's caller, which decides whether to call again
// (go-smtp retries temporary ones; go-imap stops): the loop waits for it to.
func (l *tlsListener) acceptLoop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			if l.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case l.errs <- err:
				continue
			case <-l.ctx.Done():
				return
			}
		}
		tc := tls.Server(c, l.config)
		l.wg.Go(func() { l.handshake(tc) })
	}
}

func (l *tlsListener) handshake(tc *tls.Conn) {
	ctx, cancel := context.WithTimeout(l.ctx, handshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		tc.Close()
		if l.ctx.Err() == nil { // else shutting down
			l.logFailure(tc.RemoteAddr(), err)
		}
		return
	}
	select {
	case l.conns <- tc:
	case <-l.ctx.Done():
		tc.Close()
	}
}

func (l *tlsListener) logFailure(from net.Addr, err error) {
	switch _, plain := errors.AsType[tls.RecordHeaderError](err); {
	case errors.Is(err, context.DeadlineExceeded):
		l.log.Printf("%v client %v sent no TLS handshake within %v: set up for plain text? use %v, or run serve with -plaintext",
			l.proto, from, handshakeTimeout, secureScheme(l.proto))
	case plain:
		l.log.Printf("%v client %v sent plain text to a TLS port: use %v, or run serve with -plaintext",
			l.proto, from, secureScheme(l.proto))
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// Gone without a TLS alert: the client quit, or gave up. One
		// refusing the certificate sends an alert instead.
		l.log.Printf("%v client %v closed the connection during the TLS handshake", l.proto, from)
	default:
		// A client refusing the certificate ends up here, as a remote
		// "bad certificate" or "unknown certificate authority" alert.
		l.log.Printf("%v TLS handshake with %v failed: %v (does the client trust peroxide's certificate? see 'peroxide status')",
			l.proto, from, err)
	}
}

func (l *tlsListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		return nil, err
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops accepting, ends the handshakes in progress, and waits for its
// goroutines. Connections already handed on are the server's.
func (l *tlsListener) Close() error {
	l.cancel()
	err := l.Listener.Close()
	l.wg.Wait()
	return err
}

func secureScheme(proto string) string {
	if proto == "IMAP" {
		return "imaps:// (SSL/TLS, not STARTTLS)"
	}
	return "smtps:// (SSL/TLS, not STARTTLS)"
}
