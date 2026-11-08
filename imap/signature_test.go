package imap

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/seamanticus/peroxide/protonmail"
)

// keysResponse is GET /keys for an address publishing keys.
func keysResponse(t *testing.T, keys ...*openpgp.Entity) fakeResponse {
	t.Helper()
	type pub struct {
		Send      int
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
		pubs = append(pubs, pub{1, b.String()})
	}
	body, err := json.Marshal(struct {
		Code int
		Keys []pub
	}{1000, pubs})
	if err != nil {
		t.Fatal(err)
	}
	return fakeResponse{http.StatusOK, "application/json", string(body)}
}

// builtVerdict builds msg in a new mailbox whose API answers /keys with keys,
// and returns the header field SignatureHeader.
func builtVerdict(t *testing.T, account *openpgp.Entity, msg *protonmail.Message, keys fakeResponse) string {
	t.Helper()
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{"/keys": keys}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	built, err := mbox.u.buildMessage(t.Context(), msg)
	if err != nil {
		t.Fatalf("buildMessage() = %v", err)
	}
	h, _ := splitEntity(built.literal)
	return h.Get(SignatureHeader)
}

// The body's signature is checked with the sender's keys from Proton, and
// the verdict written in X-Peroxide-Signature, RFC 8601 style: pass with the
// signing key, fail for a key that didn't make it... A body that isn't
// signed has no field, and the sender's own field is dropped.
// evidence: witnesses
func TestSignatureVerdicts(t *testing.T) {
	account, sender, stranger := newKey(t, "account"), newKey(t, "sender"), newKey(t, "stranger")
	signedBy := func(signer *openpgp.Entity) *protonmail.Message {
		msg := fetchTestMessage("a", 0)
		msg.Sender = &protonmail.MessageAddress{Address: "sender@example.org"}
		msg.Body = encryptSignedBody(t, "signed words\r\n", account, signer)
		return msg
	}
	fp := func(e *openpgp.Entity) string {
		return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
	}
	noKeys := keysResponse(t)

	for _, tc := range []struct {
		name string
		msg  *protonmail.Message
		keys fakeResponse
		want string
	}{
		{"pass", signedBy(sender), keysResponse(t, sender), "pass header.from=sender@example.org key=" + fp(sender)},
		{"by another key", signedBy(stranger), keysResponse(t, sender), "neutral (signed by a key the sender doesn't publish) header.from=sender@example.org"},
		{"sender without keys", signedBy(sender), noKeys, "permerror (Proton publishes no key for the sender) header.from=sender@example.org"},
		{"not signed", func() *protonmail.Message {
			m := fetchTestMessage("a", 0)
			m.Body = encryptBody(t, "plain\r\n", account)
			m.Header = SignatureHeader + ": pass forged\r\n"
			return m
		}(), noKeys, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := builtVerdict(t, account, tc.msg, tc.keys); got != tc.want {
				t.Errorf("%v = %q, want %q", SignatureHeader, got, tc.want)
			}
		})
	}
}

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

// The keys of a sender are asked for once, then served from the cache.
func TestSenderKeysCached(t *testing.T) {
	account, sender := newKey(t, "account"), newKey(t, "sender")
	var lookups atomic.Int32
	keys := keysResponse(t, sender)
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{"/keys": keys}}
	msgs := []*protonmail.Message{}
	for _, id := range []string{"a", "b", "c"} {
		m := fetchTestMessage(id, 0)
		m.Sender = &protonmail.MessageAddress{Address: "Sender@Example.org"}
		m.Body = encryptSignedBody(t, "signed "+id+"\r\n", account, sender)
		msgs = append(msgs, m)
	}
	mbox, _ := newActionTestMailbox(t, api, msgs)
	mbox.u.privateKeys = openpgp.EntityList{account}
	for _, m := range msgs {
		if _, err := mbox.u.buildMessage(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range api.madeRequests() {
		if r == "GET /keys" {
			lookups.Add(1)
		}
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("%v key lookups for three messages of one sender, want 1", n)
	}
}
