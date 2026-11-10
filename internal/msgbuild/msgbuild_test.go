package msgbuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/emersion/go-message/mail"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// newKey generates a key for name. Curve25519 keys are generated fast.
func newKey(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", name+"@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatalf("NewEntity(%v) = %v", name, err)
	}
	return e
}

// keysBody is the body of GET /keys for an address publishing keys.
func keysBody(t *testing.T, keys ...*openpgp.Entity) string {
	t.Helper()
	type pub struct {
		Flags     int // trusted and active, as Proton marks a sender's key
		PublicKey string
	}
	var pubs []pub
	for _, k := range keys {
		var b bytes.Buffer
		aw, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := k.Serialize(aw); err != nil {
			t.Fatal(err)
		}
		if err := aw.Close(); err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, pub{3, b.String()})
	}
	body, err := json.Marshal(struct {
		Code int
		Keys []pub
	}{1000, pubs})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A signature that doesn't verify (the text changed after signing) fails,
// naming the key that should have made it and why.
// evidence: witnesses
func TestSignatureFails(t *testing.T) {
	sender := newKey(t, "sender")
	md := &openpgp.MessageDetails{IsSigned: true, SignedBy: &openpgp.Key{PublicKey: sender.PrimaryKey}, SignatureError: errors.New("hash mismatch")}
	v := judge(md, true)
	if v.result != sigFail || v.reason != "hash mismatch" || !bytes.Equal(v.fingerprint, sender.PrimaryKey.Fingerprint) {
		t.Errorf("judge() = %+v, want fail with the reason and the key", v)
	}
}

// Two lookups of one sender at once (two of its messages built in parallel,
// as the background download does) make one request; the second waits and
// reads the cache.
func TestSenderKeysOneLookupAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sender := newKey(t, "sender")
		body := keysBody(t, sender)
		var lookups atomic.Int32
		c := &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				lookups.Add(1)
				time.Sleep(100 * time.Millisecond)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			}),
		}}
		var s SenderKeys
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				got, err := s.get(t.Context(), c, "Sender@example.org")
				if err != nil || len(got) != 1 {
					t.Errorf("get() = %v keys, %v; want 1", len(got), err)
				}
			})
		}
		wg.Wait()
		if n := lookups.Load(); n != 1 {
			t.Errorf("%v requests for two overlapping lookups, want 1", n)
		}
	})
}

// A full message's header is Proton's original one, with threading fields
// and the sent Date kept, the metadata's addresses and subject over it, and
// peroxide's own Content-Type. One that can't be parsed is ignored.
// evidence: witnesses
func TestMessageHeaderFromOriginal(t *testing.T) {
	const original = "Return-Path: <bob@example.org>\r\n" +
		"Date: Mon, 02 Nov 2026 10:00:00 +1300\r\n" +
		"From: Old Name <bob@example.org>\r\n" +
		"To: bob@protonmail.com\r\n" +
		"Subject: original subject\r\n" +
		"Message-Id: <orig@example.org>\r\n" +
		"In-Reply-To: <parent@example.org>\r\n" +
		"References: <root@example.org> <parent@example.org>\r\n" +
		"Mime-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n"

	msg := &protonmail.Message{ID: "a", LabelIDs: []string{protonmail.LabelInbox}, MIMEType: "text/plain", Body: "Hello\r\n"}
	msg.Sender = &protonmail.MessageAddress{Name: "Bob", Address: "bob@example.org"}
	msg.Subject = "decrypted subject"
	msg.Header = original
	h := MessageHeader(msg)
	for k, want := range map[string]string{
		"Return-Path": "<bob@example.org>",
		"Date":        "Mon, 02 Nov 2026 10:00:00 +1300",
		"Message-Id":  "<orig@example.org>",
		"In-Reply-To": "<parent@example.org>",
		"References":  "<root@example.org> <parent@example.org>",
		"Subject":     "decrypted subject",
		"To":          "bob@protonmail.com",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%v = %q, want %q", k, got, want)
		}
	}
	if from, err := (&mail.Header{Header: h}).AddressList("From"); err != nil || len(from) != 1 ||
		from[0].Name != "Bob" || from[0].Address != "bob@example.org" {
		t.Errorf("From = %v, %v, want the metadata's Bob <bob@example.org>", from, err)
	}
	if mt, _, err := h.ContentType(); err != nil || mt != "multipart/mixed" {
		t.Errorf("Content-Type = %q, %v, want multipart/mixed", mt, err)
	}
	if h.Has("Content-Transfer-Encoding") {
		t.Errorf("Content-Transfer-Encoding = %q, want none", h.Get("Content-Transfer-Encoding"))
	}

	msg.Header = " starts with a space\r\n"
	if h := MessageHeader(msg); h.Get("Message-Id") != "<"+messageID(msg)+">" || h.Get("Subject") != "decrypted subject" {
		t.Errorf("with a malformed header: Message-Id %q, Subject %q, want the metadata's", h.Get("Message-Id"), h.Get("Subject"))
	}
}
