package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-smtp"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

const (
	testPassword = "hunter2-secret"
	r            = protonmail.RedactedValue
)

var (
	testSASLPlain = base64.StdEncoding.EncodeToString([]byte("\x00bob\x00" + testPassword))
	testSecrets   = []string{testPassword, testSASLPlain, base64.StdEncoding.EncodeToString([]byte(testPassword))}
)

// step is data sent by the client or by the server.
type step struct {
	client bool
	data   string
}

func c(data string) step { return step{true, data} }
func s(data string) step { return step{false, data} }

type scrubberTest struct {
	name  string
	steps []step
	want  string
}

// runScrubberTests replays each test's steps through a scrubber made by
// newScrubber, in writes of various sizes, as reads and writes may split them.
func runScrubberTests(t *testing.T, tests []scrubberTest, newScrubber func(w *bytes.Buffer) (client, server func([]byte) error)) {
	t.Helper()
	for _, tc := range tests {
		for _, chunk := range []int{0, 1, 5, 23} {
			var logged bytes.Buffer
			client, server := newScrubber(&logged)
			for _, st := range tc.steps {
				write := server
				if st.client {
					write = client
				}
				for data := st.data; data != ""; {
					n := len(data)
					if chunk > 0 {
						n = min(chunk, n)
					}
					if err := write([]byte(data[:n])); err != nil {
						t.Fatalf("%v: write() = %v", tc.name, err)
					}
					data = data[n:]
				}
			}

			if got := logged.String(); got != tc.want {
				t.Errorf("%v, writes of %v bytes: logged\n%q\nwant\n%q", tc.name, chunk, got, tc.want)
			}
			for _, secret := range testSecrets {
				if strings.Contains(logged.String(), secret) {
					t.Errorf("%v, writes of %v bytes: log discloses %q", tc.name, chunk, secret)
				}
			}
		}
	}
}

func TestIMAPScrubber(t *testing.T) {
	body := "From: bob\r\n\r\na3 LOGIN bob visible\r\n"
	appendCmd := fmt.Sprintf("a1 APPEND Drafts {%d+}\r\n%s\r\n", len(body), body)

	runScrubberTests(t, []scrubberTest{{
		name:  "LOGIN",
		steps: []step{c("a1 LOGIN bob " + testPassword + "\r\n"), s("a1 OK LOGIN completed\r\n"), c("a2 SELECT INBOX\r\n")},
		want:  "a1 LOGIN " + r + "\r\na1 OK LOGIN completed\r\na2 SELECT INBOX\r\n",
	}, {
		name:  "LOGIN quoted",
		steps: []step{c(`a1 login "bob" "` + testPassword + "\"\r\n")},
		want:  "a1 login " + r + "\r\n",
	}, {
		name:  "LOGIN without tag",
		steps: []step{c("LOGIN bob " + testPassword + "\r\n")},
		want:  "LOGIN " + r + "\r\n",
	}, {
		name:  "LOGIN with extra spaces",
		steps: []step{c("a1  LOGIN\tbob  " + testPassword + "\r\n")},
		want:  "a1 LOGIN " + r + "\r\n",
	}, {
		name: "LOGIN literals",
		steps: []step{
			c("a1 LOGIN {3}\r\n"), s("+ send literal\r\n"),
			c(fmt.Sprintf("bob {%d}\r\n", len(testPassword))), s("+ send literal\r\n"),
			c(testPassword + "\r\n"), s("a1 OK\r\n"),
			c("a2 NOOP\r\n"),
		},
		want: "a1 LOGIN " + r + "\r\n+ send literal\r\n" + r + "\r\n+ send literal\r\n" + r + "\r\na1 OK\r\na2 NOOP\r\n",
	}, {
		// Pipelined commands stay hidden until the LOGIN response.
		name: "LOGIN non-synchronizing literal",
		steps: []step{
			c(fmt.Sprintf("a1 LOGIN bob {%d+}\r\n%s\r\na2 NOOP\r\n", len(testPassword), testPassword)),
			s("a1 OK\r\n"), c("a3 NOOP\r\n"),
		},
		want: "a1 LOGIN " + r + "\r\n" + r + "\r\n" + r + "\r\na1 OK\r\na3 NOOP\r\n",
	}, {
		name:  "AUTHENTICATE initial response",
		steps: []step{c("a1 AUTHENTICATE PLAIN " + testSASLPlain + "\r\n"), s("a1 OK done\r\n"), c("a2 NOOP\r\n")},
		want:  "a1 AUTHENTICATE PLAIN " + r + "\r\na1 OK done\r\na2 NOOP\r\n",
	}, {
		name: "AUTHENTICATE continuation",
		steps: []step{
			c("a1 authenticate plain\r\n"), s("+ \r\n"),
			c(testSASLPlain + "\r\n"), s("a1 OK\r\n"),
			c("a2 NOOP\r\n"),
		},
		want: "a1 authenticate plain\r\n+ \r\n" + r + "\r\na1 OK\r\na2 NOOP\r\n",
	}, {
		name:  "AUTHENTICATE cancelled",
		steps: []step{c("a1 AUTHENTICATE PLAIN\r\n"), s("+ \r\n"), c("*\r\n"), s("a1 BAD negotiation cancelled\r\n"), c("a2 LOGOUT\r\n")},
		want:  "a1 AUTHENTICATE PLAIN\r\n+ \r\n" + r + "\r\na1 BAD negotiation cancelled\r\na2 LOGOUT\r\n",
	}, {
		// Literal data isn't mistaken for commands.
		name:  "APPEND",
		steps: []step{c(appendCmd), c("a2 NOOP\r\n")},
		want:  appendCmd + "a2 NOOP\r\n",
	}, {
		// go-imap rejects '{' in an atom, so no literal follows.
		name:  "brace in atom",
		steps: []step{c("a1 SEARCH SUBJECT x{5+}\r\n"), s("* BAD atom contains forbidden char: {\r\n"), c("a2 LOGIN bob " + testPassword + "\r\n")},
		want:  "a1 SEARCH SUBJECT x{5+}\r\n* BAD atom contains forbidden char: {\r\na2 LOGIN " + r + "\r\n",
	}, {
		// The server may refuse a synchronizing literal, which is then never
		// sent: what comes next can't be skipped as its data.
		name:  "refused literal",
		steps: []step{c("a1 APPEND Drafts {20}\r\n"), s("a1 NO no such mailbox\r\n"), c("a2 LOGIN bob " + testPassword + "\r\n")},
		want:  "a1 APPEND Drafts {20}\r\na1 NO no such mailbox\r\na2 LOGIN " + r + "\r\n",
	}, {
		// A response bearing the tag inside a server literal doesn't end hiding.
		name: "server literal",
		steps: []step{
			c("a1 AUTHENTICATE PLAIN\r\n"), s("* 1 FETCH (BODY[] {7}\r\na1 OK\r\n)\r\n+ \r\n"),
			c(testSASLPlain + "\r\n"), s("a1 OK\r\n"),
		},
		want: "a1 AUTHENTICATE PLAIN\r\n* 1 FETCH (BODY[] {7}\r\na1 OK\r\n)\r\n+ \r\n" + r + "\r\na1 OK\r\n",
	}, {
		name: "AUTHENTICATE without tag",
		steps: []step{
			c("AUTHENTICATE PLAIN\r\n"), s("+ \r\n"),
			c(testSASLPlain + "\r\n"), s("AUTHENTICATE BAD unknown command\r\n"),
			c("a2 NOOP\r\n"),
		},
		want: "AUTHENTICATE PLAIN\r\n+ \r\n" + r + "\r\nAUTHENTICATE BAD unknown command\r\na2 NOOP\r\n",
	}, {
		name:  "incomplete line",
		steps: []step{c("a1 LOGIN bob " + testPassword)},
		want:  "",
	}}, func(w *bytes.Buffer) (client, server func([]byte) error) {
		sc := &imapScrubber{w: w}
		return func(p []byte) error {
				_, err := sc.writeClient(p)
				return err
			}, func(p []byte) error {
				_, err := sc.writeServer(p)
				return err
			}
	})
}

func TestSMTPScrubber(t *testing.T) {
	user := base64.StdEncoding.EncodeToString([]byte("bob"))
	password := base64.StdEncoding.EncodeToString([]byte(testPassword))
	const greeting = "220 localhost ESMTP Service Ready\r\n"

	runScrubberTests(t, []scrubberTest{{
		name: "AUTH initial response",
		steps: []step{
			s(greeting), c("EHLO client\r\n"), s("250-localhost\r\n250 AUTH PLAIN\r\n"),
			c("AUTH PLAIN " + testSASLPlain + "\r\n"), s("235 2.0.0 Authentication succeeded\r\n"),
			c("MAIL FROM:<bob@proton.me>\r\n"),
		},
		want: greeting + "EHLO client\r\n250-localhost\r\n250 AUTH PLAIN\r\nAUTH PLAIN " + r +
			"\r\n235 2.0.0 Authentication succeeded\r\nMAIL FROM:<bob@proton.me>\r\n",
	}, {
		name:  "AUTH continuation",
		steps: []step{s(greeting), c("auth plain\r\n"), s("334 \r\n"), c(testSASLPlain + "\r\n"), s("235 ok\r\n"), c("QUIT\r\n")},
		want:  greeting + "auth plain\r\n334 \r\n" + r + "\r\n235 ok\r\nQUIT\r\n",
	}, {
		name: "AUTH LOGIN",
		steps: []step{
			s(greeting), c("AUTH LOGIN\r\n"), s("334 VXNlcm5hbWU6\r\n"),
			c(user + "\r\n"), s("334 UGFzc3dvcmQ6\r\n"),
			c(password + "\r\n"), s("535 5.7.8 nope\r\n"),
			c("QUIT\r\n"),
		},
		want: greeting + "AUTH LOGIN\r\n334 VXNlcm5hbWU6\r\n" + r + "\r\n334 UGFzc3dvcmQ6\r\n" + r + "\r\n535 5.7.8 nope\r\nQUIT\r\n",
	}, {
		name:  "AUTH cancelled",
		steps: []step{s(greeting), c("AUTH PLAIN\r\n"), s("334 \r\n"), c("*\r\n"), s("501 5.0.0 Negotiation cancelled\r\n"), c("QUIT\r\n")},
		want:  greeting + "AUTH PLAIN\r\n334 \r\n" + r + "\r\n501 5.0.0 Negotiation cancelled\r\nQUIT\r\n",
	}, {
		// The reply to RSET comes after AUTH was read, but doesn't end it.
		name: "AUTH pipelined",
		steps: []step{
			s(greeting), c("RSET\r\nAUTH PLAIN\r\n"), s("250 2.0.0 ok\r\n"), s("334 \r\n"),
			c(testSASLPlain + "\r\n"), s("235 ok\r\n"), c("QUIT\r\n"),
		},
		want: greeting + "RSET\r\nAUTH PLAIN\r\n250 2.0.0 ok\r\n334 \r\n" + r + "\r\n235 ok\r\nQUIT\r\n",
	}, {
		name:  "DATA",
		steps: []step{s(greeting), c("DATA\r\n"), s("354 go ahead\r\n"), c("Subject: hi\r\n\r\nhello\r\n.\r\n"), s("250 ok\r\n")},
		want:  greeting + "DATA\r\n354 go ahead\r\nSubject: hi\r\n\r\nhello\r\n.\r\n250 ok\r\n",
	}, {
		name:  "incomplete line",
		steps: []step{s(greeting), c("AUTH PLAIN " + testSASLPlain)},
		want:  greeting,
	}}, func(w *bytes.Buffer) (client, server func([]byte) error) {
		sc := newSMTPScrubber(w)
		return sc.writeClient, sc.writeServer
	})
}

// writesLog records each Write separately.
type writesLog struct{ writes []string }

func (l *writesLog) Write(p []byte) (int, error) {
	l.writes = append(l.writes, string(p))
	return len(p), nil
}

// Other connections and the API client log to the same stream, one Write per
// entry, so each Write a scrubber makes must end a line: whatever is written
// next then starts on its own line. Literals, message data and go-imap's
// buffered writes end anywhere.
// evidence: witnesses
func TestScrubbersWriteWholeLines(t *testing.T) {
	const literal = "Subject: hi\r\n\r\nhello\r\n"
	fetch := fmt.Sprintf("* 1 FETCH (BODY[] {%d}\r\n%s)\r\na1 OK done\r\n", len(literal), literal)
	appendCmd := fmt.Sprintf("a2 APPEND Drafts {%d+}\r\n%s\r\n", len(literal), literal)

	tests := []struct {
		name   string
		write  func(w io.Writer) func(p []byte) error
		stream string
	}{
		{"IMAP server", func(w io.Writer) func([]byte) error {
			sc := &imapScrubber{w: w}
			return func(p []byte) error {
				_, err := sc.writeServer(p)
				return err
			}
		}, fetch},
		{"IMAP client literal", func(w io.Writer) func([]byte) error {
			sc := &imapScrubber{w: w}
			return func(p []byte) error {
				_, err := sc.writeClient(p)
				return err
			}
		}, appendCmd},
		{"SMTP server", func(w io.Writer) func([]byte) error {
			return newSMTPScrubber(w).writeServer
		}, "220 hello\r\n250-a\r\n250 b\r\n354 go ahead\r\n250 ok\r\n"},
	}
	for _, tt := range tests {
		for _, chunk := range []int{1, 5, 7, 23} {
			var l writesLog
			write := tt.write(&l)
			for s := tt.stream; s != ""; {
				n := min(chunk, len(s))
				if err := write([]byte(s[:n])); err != nil {
					t.Fatalf("%v: write() = %v", tt.name, err)
				}
				s = s[n:]
			}
			for _, w := range l.writes {
				if !strings.HasSuffix(w, "\n") {
					t.Errorf("%v, writes of %v bytes: wrote %q, which ends mid-line", tt.name, chunk, w)
				}
			}
			if got := strings.Join(l.writes, ""); got != tt.stream {
				t.Errorf("%v, writes of %v bytes: logged %q, want %q", tt.name, chunk, got, tt.stream)
			}
		}
	}
}

// lockedBuffer is a bytes.Buffer safe to share with server goroutines.
type lockedBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	written sync.Cond
	expired bool // set when a waitFor times out, to wake it
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written.L != nil {
		b.written.Broadcast()
	}
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// waitForTimeout bounds waitFor: far longer than any write here takes, so it
// only turns a regression that never writes into a failure instead of a hang
// until go test's own 10-minute timeout.
const waitForTimeout = 30 * time.Second

// waitFor waits until s has been written, and fails t if it isn't within
// waitForTimeout. Called from the test goroutine only.
func (b *lockedBuffer) waitFor(t *testing.T, s string) {
	t.Helper()
	timer := time.AfterFunc(waitForTimeout, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.expired = true
		if b.written.L != nil {
			b.written.Broadcast()
		}
	})
	defer timer.Stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written.L == nil {
		b.written.L = &b.mu
	}
	b.expired = false
	for !strings.Contains(b.b.String(), s) {
		if b.expired {
			t.Fatalf("%q not logged within %v; log:\n%v", s, waitForTimeout, b.b.String())
		}
		b.written.Wait()
	}
}

// pipeListener hands out in-memory connections to a server.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

// testSMTPSession accepts nothing: the stream test only greets and quits.
type testSMTPSession struct{}

func (testSMTPSession) Reset()                               {}
func (testSMTPSession) Logout() error                        { return nil }
func (testSMTPSession) Mail(string, *smtp.MailOptions) error { return smtp.ErrAuthRequired }
func (testSMTPSession) Rcpt(string, *smtp.RcptOptions) error { return smtp.ErrAuthRequired }
func (testSMTPSession) Data(io.Reader) error                 { return smtp.ErrAuthRequired }

// readReply reads one IMAP response up to its tagged line, or one SMTP reply
// up to its last line, and returns that line.
func readReply(t *testing.T, r *bufio.Reader, last func(string) bool) string {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		if last(line) {
			return line
		}
	}
}

// TestDebugSharesOneStream wires the API client, an IMAP server and an SMTP
// server as main does with -debug: all three log through log.Writer(). Their
// traffic, interleaved, must reach that one writer in the order it happened.
func TestDebugSharesOneStream(t *testing.T) {
	var logged lockedBuffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	}()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Code":1000,"Labels":[]}`))
	}))
	defer api.Close()
	c := &protonmail.Client{RootURL: api.URL, AppVersion: "Other", Debug: true}
	callAPI := func() {
		t.Helper()
		if _, err := c.ListLabels(); err != nil {
			t.Fatalf("ListLabels() = %v", err)
		}
	}

	imapServer := imapserver.New(memory.New())
	imapServer.AllowInsecureAuth = true
	imapServer.Enable(imapDebugExtension{w: log.Writer()})
	imapListener := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	imapServed := make(chan error, 1)
	go func() { imapServed <- imapServer.Serve(imapListener) }()
	defer func() {
		if err := imapServer.Close(); err != nil {
			t.Errorf("IMAP Close() = %v", err)
		}
		<-imapServed
	}()

	smtpServer := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return testSMTPSession{}, nil
	}))
	smtpServer.Domain = "localhost"
	smtpListener := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	smtpServed := make(chan error, 1)
	go func() { smtpServed <- smtpServer.Serve(smtpDebugListener{Listener: smtpListener, w: log.Writer()}) }()
	defer func() {
		if err := smtpServer.Close(); err != nil {
			t.Errorf("SMTP Close() = %v", err)
		}
		<-smtpServed
	}()

	callAPI()

	client, server := net.Pipe()
	imapListener.conns <- server
	br := bufio.NewReader(client)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read IMAP greeting: %v", err)
	}
	for _, cmd := range []string{"a1 LOGIN username password", "a2 SELECT INBOX"} {
		if _, err := client.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", cmd, err)
		}
		tag := cmd[:3]
		line := readReply(t, br, func(l string) bool { return strings.HasPrefix(l, tag) })
		// go-imap logs what it sends after sending it.
		logged.waitFor(t, line)
	}
	client.Close()

	callAPI()

	client, server = net.Pipe()
	smtpListener.conns <- server
	br = bufio.NewReader(client)
	smtpLast := func(l string) bool { return len(l) >= 4 && l[3] == ' ' }
	readReply(t, br, smtpLast)
	for _, cmd := range []string{"EHLO test", "QUIT"} {
		if _, err := client.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", cmd, err)
		}
		// smtpDebugConn logs each reply before sending it.
		readReply(t, br, smtpLast)
	}
	client.Close()

	callAPI()

	got := logged.String()
	pos := 0
	for _, want := range []string{
		">> GET /labels\n", "<< GET /labels 200 OK\n",
		"* OK ", "a1 LOGIN " + r + "\r\n", "a1 OK ", "a2 SELECT INBOX\r\n", "a2 OK ",
		">> GET /labels\n", "<< GET /labels 200 OK\n",
		"220 localhost", "EHLO test\r\n", "250 ", "QUIT\r\n", "221 ",
		">> GET /labels\n", "<< GET /labels 200 OK\n",
	} {
		i := strings.Index(got[pos:], want)
		if i < 0 {
			t.Fatalf("log has no %q after byte %v:\n%v", want, pos, got)
		}
		pos += i + len(want)
	}
	if strings.Contains(got, "username password") {
		t.Errorf("log discloses the IMAP password:\n%v", got)
	}
}

// TestIMAPDebugExtension runs a go-imap server with the extension, as
// listenAndServeIMAP does, and logs in through it.
func TestIMAPDebugExtension(t *testing.T) {
	// Credentials of go-imap's memory backend.
	const username, password = "username", "password"

	var logged lockedBuffer
	s := imapserver.New(memory.New())
	s.AllowInsecureAuth = true
	s.Enable(imapDebugExtension{w: &logged})

	l := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
		<-served
	}()

	// session sends each command and waits for its tagged response.
	session := func(cmds ...string) {
		t.Helper()
		client, server := net.Pipe()
		defer client.Close()
		l.conns <- server

		r := bufio.NewReader(client)
		if _, err := r.ReadString('\n'); err != nil {
			t.Fatalf("read greeting: %v", err)
		}
		for _, cmd := range cmds {
			if _, err := client.Write([]byte(cmd + "\r\n")); err != nil {
				t.Fatalf("write %q: %v", cmd, err)
			}
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					t.Fatalf("read response to %q: %v", cmd, err)
				}
				if strings.HasPrefix(line, "a") {
					if !strings.Contains(line, " OK ") {
						t.Fatalf("%q: got %q", cmd, line)
					}
					// go-imap logs what it sends once sent: wait for it, or
					// the next command may be read before.
					logged.waitFor(t, line)
					break
				}
				if strings.HasPrefix(line, "+") {
					// AUTHENTICATE asks for the SASL response.
					ir := base64.StdEncoding.EncodeToString([]byte("\x00" + username + "\x00" + password))
					if _, err := client.Write([]byte(ir + "\r\n")); err != nil {
						t.Fatalf("write SASL response: %v", err)
					}
				}
			}
		}
	}
	session("a1 LOGIN "+username+" "+password, "a2 SELECT INBOX")
	session("a3 AUTHENTICATE PLAIN", "a4 SELECT INBOX")

	// Client lines are logged when read, so before their response is sent.
	// go-imap reads AUTHENTICATE's SASL responses from the raw connection, so
	// they never reach the log.
	got := logged.String()
	if strings.Contains(got, password) {
		t.Errorf("debug log discloses the password:\n%v", got)
	}
	for _, want := range []string{
		"a1 LOGIN " + r + "\r\n",
		"a2 SELECT INBOX\r\n",
		"a3 AUTHENTICATE PLAIN\r\n",
		"a4 SELECT INBOX\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("debug log doesn't contain %q:\n%v", want, got)
		}
	}
}
