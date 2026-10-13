package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/emersion/go-imap"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-smtp"

	"github.com/seamanticus/peroxide/protonmail"
)

// Clients only send credentials as the arguments of IMAP's LOGIN and
// AUTHENTICATE and SMTP's AUTH, so debug logs follow each connection's
// commands and leave those arguments out. When unsure, they hide too much.

var redactedLine = []byte(protonmail.RedactedValue + "\r\n")

// eachLine calls f with every line, ending in '\n', that p completes after
// partial, and returns what is left of the last line.
func eachLine(partial, p []byte, f func(line []byte)) []byte {
	buf := append(partial, p...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		f(buf[:i+1])
		buf = buf[i+1:]
	}
	return append(partial[:0], buf...)
}

func writeLog(w io.Writer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("write debug log: %w", err)
	}
	return nil
}

// redactArgs returns the line made of fields, followed by the redaction marker
// if redacted.
func redactArgs(fields [][]byte, redacted bool) []byte {
	out := bytes.Join(fields, []byte(" "))
	if redacted {
		out = append(out, " "+protonmail.RedactedValue...)
	}
	return append(out, "\r\n"...)
}

// imapLiteral parses the literal, {n} or {n+}, that line ends with, the way
// go-imap does: it starts a field and its length fits in 32 bits.
func imapLiteral(line []byte) (n int64, nonSync, ok bool) {
	line = bytes.TrimRight(line, "\r\n")
	i := bytes.LastIndexByte(line, '{')
	if i <= 0 || (line[i-1] != ' ' && line[i-1] != '(') || !bytes.HasSuffix(line, []byte("}")) {
		return 0, false, false
	}
	digits := line[i+1 : len(line)-1]
	digits, nonSync = bytes.CutSuffix(digits, []byte("+"))
	u, err := strconv.ParseUint(string(digits), 10, 32)
	if err != nil {
		return 0, false, false
	}
	return int64(u), nonSync, true
}

// imapScrubber logs one IMAP connection. Client lines are only logged once
// complete, as their command decides what is logged.
type imapScrubber struct {
	w io.Writer

	mu sync.Mutex
	// Incomplete lines.
	client, server []byte
	// Bytes left in the literals being sent, logged as they are. Only the
	// client's non-synchronizing literals are followed: whether a synchronizing
	// one is sent depends on the server, so its data is read as lines.
	clientLiteral, serverLiteral int64
	// While set, client lines are hidden until the server's response bearing
	// this tag, which comes after every line of the command was read.
	hideTag string
}

func (s *imapScrubber) writeClient(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []byte
	buf := append(s.client, p...)
	for len(buf) > 0 {
		if s.clientLiteral > 0 {
			n := int(min(s.clientLiteral, int64(len(buf))))
			out = append(out, buf[:n]...)
			s.clientLiteral -= int64(n)
			buf = buf[n:]
			continue
		}

		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		out = append(out, s.clientLine(buf[:i+1])...)
		buf = buf[i+1:]
	}
	s.client = append(s.client[:0], buf...)
	return len(p), writeLog(s.w, out)
}

func (s *imapScrubber) clientLine(line []byte) []byte {
	if s.hideTag != "" {
		return redactedLine
	}

	fields := bytes.Fields(line)
	// The command follows the tag, but a client may leave the tag out. go-imap
	// then takes the command as the tag, which its response bears.
	for i := 0; i < min(2, len(fields)); i++ {
		switch strings.ToUpper(string(fields[i])) {
		case "LOGIN":
			// Credentials sent as literals follow on the next lines.
			if bytes.HasSuffix(bytes.TrimRight(line, "\r\n"), []byte("}")) {
				s.hideTag = string(fields[0])
			}
			return redactArgs(fields[:i+1], true)
		case "AUTHENTICATE":
			// SASL responses follow on the next lines. Keep the mechanism.
			s.hideTag = string(fields[0])
			n := min(i+2, len(fields))
			return redactArgs(fields[:n], len(fields) > n)
		}
	}

	if n, nonSync, ok := imapLiteral(line); ok && nonSync {
		s.clientLiteral = n
	}
	return line
}

func (s *imapScrubber) writeServer(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Only followed to find the response ending hiding: literals, such as
	// message bodies, are skipped as they may hold anything.
	buf := append(s.server, p...)
	for len(buf) > 0 {
		if s.serverLiteral > 0 {
			n := int(min(s.serverLiteral, int64(len(buf))))
			s.serverLiteral -= int64(n)
			buf = buf[n:]
			continue
		}

		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := buf[:i+1]
		if s.hideTag != "" && bytes.HasPrefix(line, []byte(s.hideTag+" ")) {
			s.hideTag = ""
		}
		if n, _, ok := imapLiteral(line); ok {
			s.serverLiteral = n
		}
		buf = buf[i+1:]
	}
	s.server = append(s.server[:0], buf...)
	return len(p), writeLog(s.w, p)
}

// The two directions of an IMAP connection, as writers. They must be distinct
// comparable types: go-imap compares them with ==.
type (
	imapClientLog struct{ *imapScrubber }
	imapServerLog struct{ *imapScrubber }
)

func (l imapClientLog) Write(p []byte) (int, error) { return l.writeClient(p) }

func (l imapServerLog) Write(p []byte) (int, error) { return l.writeServer(p) }

// imapDebugExtension gives each IMAP connection its own scrubber.
type imapDebugExtension struct {
	w io.Writer
}

func (imapDebugExtension) Capabilities(imapserver.Conn) []string { return nil }

func (imapDebugExtension) Command(string) imapserver.HandlerFactory { return nil }

func (ext imapDebugExtension) NewConn(c imapserver.Conn) imapserver.Conn {
	// Server connections embed *imap.Conn, which has SetDebug.
	d, ok := c.(interface{ SetDebug(io.Writer) })
	if !ok {
		log.Printf("cannot log IMAP traffic: %T has no SetDebug", c)
		return c
	}
	s := &imapScrubber{w: ext.w}
	d.SetDebug(imap.NewDebugWriter(imapServerLog{s}, imapClientLog{s}))
	return c
}

// smtpScrubber logs one SMTP connection. Client lines are only logged once
// complete, as their command decides what is logged.
//
// SMTP replies bear no tag and clients may pipeline commands, so the AUTH
// exchange is found by counting: every client line gets one reply, the last
// line of which has a space after its code.
type smtpScrubber struct {
	w io.Writer

	mu             sync.Mutex
	client, server []byte // incomplete lines
	// Replies owed and received. The greeting is owed to the connection.
	// Lines owing no reply, such as message data, only make the AUTH exchange
	// look longer.
	owed, received int
	// While hiding, client lines are hidden until a reply other than 334 to a
	// line from the authReply-th on.
	hiding    bool
	authReply int
}

func newSMTPScrubber(w io.Writer) *smtpScrubber {
	return &smtpScrubber{w: w, owed: 1}
}

func (s *smtpScrubber) writeClient(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []byte
	s.client = eachLine(s.client, p, func(line []byte) {
		out = append(out, s.clientLine(line)...)
	})
	return writeLog(s.w, out)
}

func (s *smtpScrubber) clientLine(line []byte) []byte {
	s.owed++
	if s.hiding {
		// A SASL response, "*" cancelling the exchange, or a command.
		return redactedLine
	}

	fields := bytes.Fields(line)
	if len(fields) == 0 || !strings.EqualFold(string(fields[0]), "AUTH") {
		return line
	}
	s.hiding = true
	s.authReply = s.owed
	// Keep the mechanism.
	n := min(2, len(fields))
	return redactArgs(fields[:n], len(fields) > n)
}

func (s *smtpScrubber) writeServer(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.server = eachLine(s.server, p, func(line []byte) {
		line = bytes.TrimRight(line, "\r\n")
		if len(line) < 3 || (len(line) > 3 && line[3] != ' ') {
			return // not the last line of a reply
		}
		s.received++
		if s.hiding && s.received >= s.authReply && !bytes.HasPrefix(line, []byte("334")) {
			s.hiding = false
		}
	})
	return writeLog(s.w, p)
}

// smtpDebugConn logs the traffic of an SMTP connection.
type smtpDebugConn struct {
	net.Conn
	s *smtpScrubber
}

func (c *smtpDebugConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		if err := c.s.writeClient(p[:n]); err != nil {
			return n, err
		}
	}
	return n, err
}

func (c *smtpDebugConn) Write(p []byte) (int, error) {
	// Logged first: once the client has a reply, its next line may be read,
	// and the reply must have been counted by then.
	if err := c.s.writeServer(p); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

type smtpDebugListener struct {
	net.Listener
	w io.Writer
}

func (l smtpDebugListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &smtpDebugConn{Conn: c, s: newSMTPScrubber(l.w)}, nil
}

// serveSMTPDebug serves s, logging its traffic to w. go-smtp's own Debug writer
// is shared by all connections, which can't be followed apart, so connections
// are tapped instead, above TLS.
func serveSMTPDebug(s *smtp.Server, w io.Writer) error {
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen for SMTP: %w", err)
	}

	if s.TLSConfig == nil {
		log.Println("SMTP server listening on", s.Addr)
	} else {
		l = tls.NewListener(l, s.TLSConfig)
		// Every connection uses TLS, but go-smtp can't tell through the tap.
		s.TLSConfig = nil
		s.AllowInsecureAuth = true
		log.Println("SMTP server listening with TLS on", s.Addr)
	}
	return s.Serve(smtpDebugListener{Listener: l, w: w})
}
