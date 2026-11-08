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
	"github.com/emersion/go-message"

	"github.com/seamanticus/peroxide/protonmail"
)

// keysResponse is GET /keys for an address publishing keys.
func keysResponse(t *testing.T, keys ...*openpgp.Entity) fakeResponse {
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
	return fakeResponse{http.StatusOK, "application/json", string(body)}
}

// untrusted is GET /keys for an address whose only key its owner marked
// compromised: Flags without Trusted, as Proton answers then.
func untrusted(t *testing.T, key *openpgp.Entity) fakeResponse {
	t.Helper()
	r := keysResponse(t, key)
	r.body = strings.Replace(r.body, `"Flags":3`, `"Flags":2`, 1)
	return r
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
		{"key marked compromised", signedBy(sender), untrusted(t, sender), "permerror (Proton publishes no key for the sender) header.from=sender@example.org"},
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

// signAttachment sets in att the detached signature of data by signer, as
// Proton's clients make it.
func signAttachment(t *testing.T, att *protonmail.Attachment, signer *openpgp.Entity, data string) {
	t.Helper()
	if _, err := att.Sign(signer, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

// Each attachment's detached signature is checked over its decrypted bytes:
// its part carries its own X-Peroxide-Signature, and the message's field
// sums them up in attachments=, the worst first. An unsigned attachment has
// no field and counts as neutral beside signed ones; a body not signed
// beside a signed attachment is "none".
// evidence: witnesses
func TestAttachmentSignatures(t *testing.T) {
	account, sender, stranger := newKey(t, "account"), newKey(t, "sender"), newKey(t, "stranger")
	good, goodData := encryptAttachment(t, "att1", "a", "good.txt", "good file", account)
	signAttachment(t, good, sender, "good file")
	changed, changedData := encryptAttachment(t, "att2", "a", "changed.txt", "the file sent", account)
	signAttachment(t, changed, sender, "another file")
	other, otherData := encryptAttachment(t, "att3", "a", "other.txt", "other file", account)
	signAttachment(t, other, stranger, "other file")
	plain, plainData := encryptAttachment(t, "att4", "a", "plain.txt", "plain file", account)

	for _, tc := range []struct {
		name     string
		atts     []*protonmail.Attachment
		signBody bool
		want     string   // the message's field, without the key
		parts    []string // each attachment part's field, its prefix
	}{
		{"all pass", []*protonmail.Attachment{good}, true, "pass header.from=sender@example.org key=* attachments=pass", []string{"pass key="}},
		{"one changed", []*protonmail.Attachment{good, changed}, true, "pass header.from=sender@example.org key=* attachments=fail", []string{"pass key=", "fail ("}},
		{"another key", []*protonmail.Attachment{other}, true, "pass header.from=sender@example.org key=* attachments=neutral", []string{"neutral ("}},
		{"one unsigned", []*protonmail.Attachment{good, plain}, true, "pass header.from=sender@example.org key=* attachments=neutral", []string{"pass key=", ""}},
		{"none signed", []*protonmail.Attachment{plain}, true, "pass header.from=sender@example.org key=* attachments=none", []string{""}},
		{"only an attachment signed", []*protonmail.Attachment{good}, false, "none header.from=sender@example.org attachments=pass", []string{"pass key="}},
		{"nothing signed", []*protonmail.Attachment{plain}, false, "", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := fetchTestMessage("a", 0)
			msg.Sender = &protonmail.MessageAddress{Address: "sender@example.org"}
			if tc.signBody {
				msg.Body = encryptSignedBody(t, "body\r\n", account, sender)
			} else {
				msg.Body = encryptBody(t, "body\r\n", account)
			}
			msg.Attachments = tc.atts
			msg.NumAttachments = len(tc.atts)
			gets := map[string]fakeResponse{"/keys": keysResponse(t, sender)}
			for id, data := range map[string]string{"att1": goodData, "att2": changedData, "att3": otherData, "att4": plainData} {
				gets["/attachments/"+id] = fakeResponse{http.StatusOK, "application/octet-stream", data}
			}
			mbox, _ := newActionTestMailbox(t, &fakeAPI{t: t, gets: gets}, []*protonmail.Message{msg})
			mbox.u.privateKeys = openpgp.EntityList{account}
			built, err := mbox.u.buildMessage(t.Context(), msg)
			if err != nil {
				t.Fatalf("buildMessage() = %v", err)
			}

			h, _ := splitEntity(built.literal)
			got := h.Get(SignatureHeader)
			if i := strings.Index(got, "key="); i >= 0 {
				if j := strings.IndexByte(got[i:], ' '); j >= 0 {
					got = got[:i] + "key=*" + got[i+j:]
				}
			}
			if got != tc.want {
				t.Errorf("message field %q, want %q", got, tc.want)
			}

			e, err := message.Read(bytes.NewReader(built.literal))
			if err != nil {
				t.Fatal(err)
			}
			mr := e.MultipartReader()
			if _, err := mr.NextPart(); err != nil { // the body
				t.Fatal(err)
			}
			for i, want := range tc.parts {
				p, err := mr.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				got := p.Header.Get(SignatureHeader)
				if (want == "") != (got == "") || !strings.HasPrefix(got, want) {
					t.Errorf("attachment %v field %q, want %q...", i+1, got, want)
				}
			}
		})
	}
}

// A signature made with Sign reads back with SignaturePacket, base64 as
// Proton returns it or armored.
// evidence: witnesses
func TestAttachmentSignatureFormats(t *testing.T) {
	signer := newKey(t, "signer")
	att := &protonmail.Attachment{ID: "a", Name: "f"}
	raw, err := att.Sign(signer, strings.NewReader("file"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := att.SignaturePacket(); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("SignaturePacket() of base64 = %x, %v; want %x", got, err, raw)
	}
	var arm bytes.Buffer
	w, _ := armor.Encode(&arm, "PGP SIGNATURE", nil)
	w.Write(raw)
	w.Close()
	att.Signature = arm.String()
	if got, err := att.SignaturePacket(); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("SignaturePacket() of armor = %x, %v; want %x", got, err, raw)
	}
	att.Signature = ""
	if got, err := att.SignaturePacket(); err != nil || got != nil {
		t.Errorf("SignaturePacket() of none = %x, %v; want nil", got, err)
	}
	att.Signature = "!!"
	if _, err := att.SignaturePacket(); err == nil {
		t.Error("SignaturePacket() of garbage succeeded")
	}
}
