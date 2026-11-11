package msgbuild

import (
	"bytes"
	"mime"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// sealBody encrypts plaintext to to, signed by signer unless nil, armored as
// Proton stores a body.
func sealBody(t *testing.T, plaintext []byte, to, signer *openpgp.Entity) string {
	t.Helper()
	var b bytes.Buffer
	aw, err := armor.Encode(&b, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, openpgp.EntityList{to}, signer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The Sent copy of a message the client encrypted itself is served as that
// message: the client's Content-Type and body byte for byte, under the
// header built from Proton's metadata, and no verdict field of the
// client's. Only a text/plain draft or sent message of the account, signed
// by its key, whose body is the marker and a multipart/encrypted entity,
// is; anything else is built as usual.
// evidence: witnesses
func TestClientPGPMIME(t *testing.T) {
	account, stranger := newKey(t, "account"), newKey(t, "stranger")
	const data = "--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"\r\n--b\r\nContent-Type: application/octet-stream\r\n\r\n" +
		"-----BEGIN PGP MESSAGE-----\r\n\r\nwcBMA\r\n-----END PGP MESSAGE-----\r\n\r\n--b--\r\n"
	const entity = "From: account@example.org\r\n" +
		"Subject: client subject\r\n" +
		SignatureHeader + ": pass\r\n" +
		"Content-Type: multipart/encrypted; boundary=b; protocol=\"application/pgp-encrypted\"\r\n" +
		"\r\n" + data
	marked := ClientPGPMIMEBody([]byte(entity))

	newMsg := func(typ protonmail.MessageType, body []byte, signer *openpgp.Entity) *protonmail.Message {
		return &protonmail.Message{
			ID: "a", Type: typ, MIMEType: "text/plain", Subject: "metadata subject",
			Sender: &protonmail.MessageAddress{Address: "account@example.org"},
			ToList: []*protonmail.MessageAddress{{Address: "bob@example.org"}},
			Body:   sealBody(t, body, account, signer),
		}
	}
	mb := &Builder{Keys: openpgp.EntityList{account}}

	for _, tc := range []struct {
		name string
		msg  *protonmail.Message
		want bool
	}{
		{"sent", newMsg(protonmail.MessageSent, marked, account), true},
		{"draft", newMsg(protonmail.MessageDraft, marked, account), true},
		{"marker line ending in LF", newMsg(protonmail.MessageSent, []byte(clientPGPMIMEMarker+"\n"+entity), account), true},
		{"received", newMsg(protonmail.MessageInbox, marked, account), false},
		{"sent to self", newMsg(protonmail.MessageInboxAndSent, marked, account), false},
		{"signed by another key", newMsg(protonmail.MessageSent, marked, stranger), false},
		{"unsigned", newMsg(protonmail.MessageSent, marked, nil), false},
		{"no marker", newMsg(protonmail.MessageSent, []byte(entity), account), false},
		{"marker not on a line of its own", newMsg(protonmail.MessageSent, []byte(clientPGPMIMEMarker+" "+entity), account), false},
		{"not PGP/MIME", newMsg(protonmail.MessageSent, ClientPGPMIMEBody([]byte("Content-Type: text/plain\r\n\r\nhi\r\n")), account), false},
		{"html", func() *protonmail.Message {
			m := newMsg(protonmail.MessageSent, marked, account)
			m.MIMEType = "text/html"
			return m
		}(), false},
		{"zero message", &protonmail.Message{}, false},
	} {
		literal, ok := mb.clientPGPMIME(tc.msg)
		if ok != tc.want {
			t.Errorf("%s: clientPGPMIME() ok = %v, want %v", tc.name, ok, tc.want)
			continue
		}
		if !ok {
			continue
		}
		h, body := SplitEntity(literal)
		if mt, params, err := mime.ParseMediaType(h.Get("Content-Type")); err != nil || mt != "multipart/encrypted" || params["boundary"] != "b" {
			t.Errorf("%s: Content-Type = %q, want the client's", tc.name, h.Get("Content-Type"))
		}
		if string(body) != data {
			t.Errorf("%s: body = %q, want the client's byte for byte", tc.name, body)
		}
		if got := h.Get("Subject"); got != "metadata subject" {
			t.Errorf("%s: Subject = %q, want the metadata's", tc.name, got)
		}
		if h.Has(SignatureHeader) {
			t.Errorf("%s: %s = %q, want none: only peroxide writes it", tc.name, SignatureHeader, h.Get(SignatureHeader))
		}
	}

	// Build serves it so, as a message to keep (not a fallback).
	literal, fallback, err := mb.Build(t.Context(), newMsg(protonmail.MessageSent, marked, account))
	if err != nil || fallback || !strings.HasSuffix(string(literal), data) {
		t.Errorf("Build() = %q, %v, %v; want the client's message, no fallback", literal, fallback, err)
	}
}
