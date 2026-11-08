package sendrecord

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A send is found once sent, by its user, hash and recipients in any order;
// a failed one is forgotten; one under way is waited for.
// evidence: witnesses
func TestSentMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		r := New()

		r.Start("bob", "h", []string{"a", "b"}).Sent("m1")
		r.Start("bob", "h2", []string{"a"}).Failed()
		for _, tt := range []struct {
			name, user, hash string
			to               []string
			wantID           string
		}{
			{"sent", "bob", "h", []string{"a", "b"}, "m1"},
			{"recipients reordered", "bob", "h", []string{"b", "a"}, "m1"},
			{"fewer recipients", "bob", "h", []string{"a"}, ""},
			{"more recipients", "bob", "h", []string{"a", "b", "c"}, ""},
			{"another user", "ann", "h", []string{"a", "b"}, ""},
			{"another message", "bob", "x", []string{"a", "b"}, ""},
			{"failed", "bob", "h2", []string{"a"}, ""},
		} {
			id, ok := r.SentMessage(ctx, tt.user, tt.hash, tt.to)
			if id != tt.wantID || ok != (tt.wantID != "") {
				t.Errorf("%v: SentMessage() = %q, %v, want %q", tt.name, id, ok, tt.wantID)
			}
		}

		// Under way: the lookup waits for the send to end.
		send := r.Start("bob", "h3", []string{"a"})
		got := make(chan string)
		go func() {
			id, _ := r.SentMessage(ctx, "bob", "h3", []string{"a"})
			got <- id
		}()
		synctest.Wait()
		send.Sent("m3")
		if id := <-got; id != "m3" {
			t.Errorf("SentMessage() after waiting = %q, want m3", id)
		}

		// A send that fails is waited for, then not found.
		send = r.Start("bob", "h4", []string{"a"})
		go func() {
			id, _ := r.SentMessage(ctx, "bob", "h4", []string{"a"})
			got <- id
		}()
		synctest.Wait()
		send.Failed()
		if id := <-got; id != "" {
			t.Errorf("SentMessage() of a failed send = %q, want none", id)
		}
	})
}

// A wait ends with its context; a send is forgotten after Expiry.
// evidence: witnesses
func TestSentMessageTimes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := New()
		r.Start("bob", "pending", []string{"a"})
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if _, ok := r.SentMessage(ctx, "bob", "pending", []string{"a"}); ok {
			t.Error("a send under way found once the wait timed out")
		}

		r.Start("bob", "h", []string{"a"}).Sent("m1")
		time.Sleep(Expiry - time.Second)
		if _, ok := r.SentMessage(t.Context(), "bob", "h", []string{"a"}); !ok {
			t.Error("a send not found before Expiry")
		}
		time.Sleep(2 * time.Second)
		if _, ok := r.SentMessage(t.Context(), "bob", "h", []string{"a"}); ok {
			t.Error("a send found after Expiry")
		}
		// The pending send expired too: a lookup doesn't wait for it.
		if _, ok := r.SentMessage(t.Context(), "bob", "pending", []string{"a"}); ok {
			t.Error("an expired send under way found")
		}
	})
}

// Hash is the same for the forms a client gives one message, over SMTP and
// with APPEND: boundaries, transfer encodings, header fields but the hashed
// ones and their order may differ; the text, subject and recipients may not.
// evidence: witnesses
func TestHash(t *testing.T) {
	const sent = "From: Bob <bob@example.org>\r\n" +
		"To: ann@example.org\r\n" +
		"Subject: hi\r\n" +
		"Content-Type: multipart/mixed; boundary=one\r\n" +
		"\r\n" +
		"--one\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"caf=C3=A9\r\n" +
		"--one\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=a.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"AAEC\r\n" +
		"--one--\r\n"
	const saved = "Subject: hi\r\n" +
		"Date: Mon, 2 Nov 2026 10:00:00 +1300\r\n" +
		"Message-Id: <x@example.org>\r\n" +
		"Bcc: secret@example.org\r\n" +
		"To: Ann <ann@example.org>\r\n" +
		"From: bob@example.org\r\n" +
		"Content-Type: multipart/mixed; boundary=\"two\"\r\n" +
		"\r\n" +
		"--two\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"Y2Fmw6k=\r\n" +
		"--two\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=a.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"AAEC\r\n" +
		"--two--\r\n"
	want, err := Hash([]byte(sent))
	if err != nil {
		t.Fatalf("Hash() = %v", err)
	}
	if got, err := Hash([]byte(saved)); err != nil || got != want {
		t.Errorf("Hash() of the saved copy = %q, %v, want the sent one's %q", got, err, want)
	}
	for _, tt := range []struct{ name, old, new string }{
		{"subject", "Subject: hi", "Subject: ho"},
		{"recipient", "To: ann@example.org", "To: carol@example.org"},
		{"text", "caf=C3=A9", "cafe"},
		{"attachment", "AAEC", "AAED"},
		{"filename", "filename=a.bin", "filename=b.bin"},
	} {
		other := strings.Replace(sent, tt.old, tt.new, 1)
		if got, err := Hash([]byte(other)); err != nil || got == want {
			t.Errorf("%v changed: Hash() = %q, %v, want another hash", tt.name, got, err)
		}
	}
}
