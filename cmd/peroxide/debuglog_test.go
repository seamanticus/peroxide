package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"

	"github.com/seamanticus/peroxide/protonmail"
)

const (
	testPassword = "hunter2-secret"
	r            = protonmail.RedactedValue
)

var (
	testSASLPlain = base64.StdEncoding.EncodeToString([]byte("\x00jon\x00" + testPassword))
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
	body := "From: jon\r\n\r\na3 LOGIN jon visible\r\n"
	appendCmd := fmt.Sprintf("a1 APPEND Drafts {%d+}\r\n%s\r\n", len(body), body)

	runScrubberTests(t, []scrubberTest{{
		name:  "LOGIN",
		steps: []step{c("a1 LOGIN jon " + testPassword + "\r\n"), s("a1 OK LOGIN completed\r\n"), c("a2 SELECT INBOX\r\n")},
		want:  "a1 LOGIN " + r + "\r\na1 OK LOGIN completed\r\na2 SELECT INBOX\r\n",
	}, {
		name:  "LOGIN quoted",
		steps: []step{c(`a1 login "jon" "` + testPassword + "\"\r\n")},
		want:  "a1 login " + r + "\r\n",
	}, {
		name:  "LOGIN without tag",
		steps: []step{c("LOGIN jon " + testPassword + "\r\n")},
		want:  "LOGIN " + r + "\r\n",
	}, {
		name:  "LOGIN with extra spaces",
		steps: []step{c("a1  LOGIN\tjon  " + testPassword + "\r\n")},
		want:  "a1 LOGIN " + r + "\r\n",
	}, {
		name: "LOGIN literals",
		steps: []step{
			c("a1 LOGIN {3}\r\n"), s("+ send literal\r\n"),
			c(fmt.Sprintf("jon {%d}\r\n", len(testPassword))), s("+ send literal\r\n"),
			c(testPassword + "\r\n"), s("a1 OK\r\n"),
			c("a2 NOOP\r\n"),
		},
		want: "a1 LOGIN " + r + "\r\n+ send literal\r\n" + r + "\r\n+ send literal\r\n" + r + "\r\na1 OK\r\na2 NOOP\r\n",
	}, {
		// Pipelined commands stay hidden until the LOGIN response.
		name: "LOGIN non-synchronizing literal",
		steps: []step{
			c(fmt.Sprintf("a1 LOGIN jon {%d+}\r\n%s\r\na2 NOOP\r\n", len(testPassword), testPassword)),
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
		steps: []step{c("a1 SEARCH SUBJECT x{5+}\r\n"), s("* BAD atom contains forbidden char: {\r\n"), c("a2 LOGIN jon " + testPassword + "\r\n")},
		want:  "a1 SEARCH SUBJECT x{5+}\r\n* BAD atom contains forbidden char: {\r\na2 LOGIN " + r + "\r\n",
	}, {
		// The server may refuse a synchronizing literal, which is then never
		// sent: what comes next can't be skipped as its data.
		name:  "refused literal",
		steps: []step{c("a1 APPEND Drafts {20}\r\n"), s("a1 NO no such mailbox\r\n"), c("a2 LOGIN jon " + testPassword + "\r\n")},
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
		steps: []step{c("a1 LOGIN jon " + testPassword)},
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
	user := base64.StdEncoding.EncodeToString([]byte("jon"))
	password := base64.StdEncoding.EncodeToString([]byte(testPassword))
	const greeting = "220 localhost ESMTP Service Ready\r\n"

	runScrubberTests(t, []scrubberTest{{
		name: "AUTH initial response",
		steps: []step{
			s(greeting), c("EHLO client\r\n"), s("250-localhost\r\n250 AUTH PLAIN\r\n"),
			c("AUTH PLAIN " + testSASLPlain + "\r\n"), s("235 2.0.0 Authentication succeeded\r\n"),
			c("MAIL FROM:<jon@proton.me>\r\n"),
		},
		want: greeting + "EHLO client\r\n250-localhost\r\n250 AUTH PLAIN\r\nAUTH PLAIN " + r +
			"\r\n235 2.0.0 Authentication succeeded\r\nMAIL FROM:<jon@proton.me>\r\n",
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

// lockedBuffer is a bytes.Buffer safe to share with server goroutines.
type lockedBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	written sync.Cond
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

// waitFor waits until s has been written.
func (b *lockedBuffer) waitFor(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written.L == nil {
		b.written.L = &b.mu
	}
	for !strings.Contains(b.b.String(), s) {
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
					logged.waitFor(line)
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
