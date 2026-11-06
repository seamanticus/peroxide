package imap

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-message"

	"github.com/seamanticus/peroxide/protonmail"
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

// encryptBody returns plaintext encrypted to to, armored as Proton stores it.
func encryptBody(t *testing.T, plaintext string, to *openpgp.Entity) string {
	t.Helper()
	var b bytes.Buffer
	aw, err := armor.Encode(&b, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, []*openpgp.Entity{to}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, plaintext); err != nil {
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

func fetchItems(t *testing.T, mbox *mailbox, items ...imap.FetchItem) *imap.Message {
	t.Helper()
	msgs, err := fetchAll(t, mbox, items...)
	if err != nil {
		t.Fatalf("FETCH %v: %v", items, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("FETCH %v: sent %v messages, want 1", items, len(msgs))
	}
	return msgs[0]
}

// sectionBytes returns the body section item of fetched. The server keys
// sections by the name requested, .PEEK included; imap.Message.GetBody strips
// .PEEK, as clients receive them, so it can't find them here.
func sectionBytes(t *testing.T, fetched *imap.Message, item imap.FetchItem) []byte {
	t.Helper()
	want, err := imap.ParseBodySectionName(item)
	if err != nil {
		t.Fatal(err)
	}
	for section, l := range fetched.Body {
		if !section.Equal(want) {
			continue
		}
		b, err := io.ReadAll(l)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("no %v in the response", item)
	return nil
}

// A body encrypted end to end to a key the account doesn't have reaches the
// client unchanged, as PGP/MIME (RFC 3156), with a BODYSTRUCTURE that says so;
// one encrypted to the account's key is decrypted, as before.
func TestEncryptedToOtherKeyPassedThrough(t *testing.T) {
	account, recipient := newKey(t, "account"), newKey(t, "recipient")
	armored := encryptBody(t, "Content-Type: text/plain\r\n\r\nsecret\r\n", recipient)

	msg := fetchTestMessage("a", 0)
	msg.MIMEType = "multipart/mixed"
	msg.IsEncrypted = protonmail.MessageEncryptedPGPMIME
	msg.Body = armored
	api := &fakeAPI{t: t}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	cacheFull(mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, "BODY.PEEK[]", "BODY.PEEK[2]")

	bs := fetched.BodyStructure
	if bs.MIMEType != "multipart" || bs.MIMESubType != "encrypted" || bs.Params["protocol"] != "application/pgp-encrypted" {
		t.Errorf("BODYSTRUCTURE %v/%v %v, want multipart/encrypted with protocol application/pgp-encrypted", bs.MIMEType, bs.MIMESubType, bs.Params)
	}
	if len(bs.Parts) != 2 ||
		bs.Parts[0].MIMEType+"/"+bs.Parts[0].MIMESubType != "application/pgp-encrypted" ||
		bs.Parts[1].MIMEType+"/"+bs.Parts[1].MIMESubType != "application/octet-stream" {
		t.Errorf("BODYSTRUCTURE parts %+v, want application/pgp-encrypted then application/octet-stream", bs.Parts)
	}

	// The data part is the stored message, byte for byte.
	if got := string(sectionBytes(t, fetched, "BODY.PEEK[2]")); got != armored {
		t.Errorf("BODY[2] differs from the stored body:\n%q\nwant\n%q", got, armored)
	}

	// The whole message parses as PGP/MIME and opens with the recipient key.
	whole := sectionBytes(t, fetched, "BODY.PEEK[]")
	e, err := message.Read(bytes.NewReader(whole))
	if err != nil {
		t.Fatalf("BODY[] doesn't parse: %v", err)
	}
	mt, params, _ := e.Header.ContentType()
	if mt != "multipart/encrypted" || params["protocol"] != "application/pgp-encrypted" {
		t.Errorf("BODY[] Content-Type %v %v, want multipart/encrypted", mt, params)
	}
	mr := e.MultipartReader()
	var parts []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p.Body)
		parts = append(parts, string(b))
	}
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != "Version: 1" {
		t.Fatalf("BODY[] parts %q, want the version part and the data", parts)
	}
	block, err := armor.Decode(strings.NewReader(parts[1]))
	if err != nil {
		t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{recipient}, nil, nil)
	if err != nil {
		t.Fatalf("recipient can't open BODY[]: %v", err)
	}
	if b, _ := io.ReadAll(md.UnverifiedBody); !strings.Contains(string(b), "secret") {
		t.Errorf("recipient read %q, want the plaintext", b)
	}

	// The same FETCH twice gives the same bytes: the boundary is stable.
	if again := sectionBytes(t, fetchItems(t, mbox, "BODY.PEEK[]"), "BODY.PEEK[]"); !bytes.Equal(again, whole) {
		t.Error("two FETCH BODY[] gave different bytes")
	}

	if got := api.madeRequests(); len(got) != 0 {
		t.Errorf("made requests %v, want none: the message was cached", got)
	}
}

// A body Proton encrypted to the account's key is decrypted, whatever
// IsEncrypted says, and BODYSTRUCTURE describes the part as it is sent.
func TestBodyStructureOfDecryptedBody(t *testing.T) {
	account := newKey(t, "account")
	msg := fetchTestMessage("a", 0)
	msg.MIMEType = "text/plain"
	msg.IsEncrypted = protonmail.MessageEncryptedExternal // "on-delivery"
	msg.Body = encryptBody(t, "This is an unencrypted message\r\n", account)
	api := &fakeAPI{t: t}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	cacheFull(mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, "BODY.PEEK[1]")
	part := sectionBytes(t, fetched, "BODY.PEEK[1]")
	if !strings.Contains(string(part), "This is an unencrypted message") {
		t.Errorf("BODY[1] = %q, want the decrypted text", part)
	}

	bs := fetched.BodyStructure
	if bs.MIMEType != "multipart" || len(bs.Parts) != 1 {
		t.Fatalf("BODYSTRUCTURE %v/%v with %v parts, want multipart with 1", bs.MIMEType, bs.MIMESubType, len(bs.Parts))
	}
	inline := bs.Parts[0]
	if got := inline.MIMEType + "/" + inline.MIMESubType; got != "text/plain" {
		t.Errorf("inline part is %v, want text/plain", got)
	}
	if inline.Size != uint32(len(part)) {
		t.Errorf("inline part size %v, want %v, the length of BODY[1]", inline.Size, len(part))
	}
	if want := uint32(bytes.Count(part, []byte("\n"))); inline.Lines != want {
		t.Errorf("inline part lines %v, want %v", inline.Lines, want)
	}
}

// Bodies that aren't PGP messages, which Proton doesn't store but older
// drafts might, are served as they are.
func TestReadPlainBody(t *testing.T) {
	for _, body := range []string{"", "plain text\r\n", "  not -----BEGIN PGP MESSAGE----- at the start"} {
		msg := &protonmail.Message{Body: body, IsEncrypted: protonmail.MessageEncryptedInternal}
		md, err := msg.Read(nil, nil)
		if err != nil {
			t.Fatalf("Read(%q) = %v", body, err)
		}
		if got, _ := io.ReadAll(md.UnverifiedBody); string(got) != body {
			t.Errorf("Read(%q) gave %q", body, got)
		}
	}
}

// publicKey returns e as the API lists an account key: armored, Active.
func publicKey(t *testing.T, id string, e *openpgp.Entity) *protonmail.PrivateKey {
	t.Helper()
	var b bytes.Buffer
	aw, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(aw); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return &protonmail.PrivateKey{ID: id, PrivateKey: b.String(), Active: 1}
}

// encryptAttachment returns an attachment of msgID holding data, encrypted to
// to, and the ciphertext the API serves for it.
func encryptAttachment(t *testing.T, id, msgID, name, data string, to *openpgp.Entity) (*protonmail.Attachment, string) {
	t.Helper()
	att := &protonmail.Attachment{ID: id, MessageID: msgID, Name: name, MIMEType: "text/plain", Size: len(data)}
	if _, err := att.GenerateKey([]*openpgp.Entity{to}); err != nil {
		t.Fatal(err)
	}
	var ct bytes.Buffer
	w, err := att.Encrypt(&ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return att, ct.String()
}

// readArmored opens the armored PGP message s with keys, and returns its
// plaintext and armor Comment.
func readArmored(t *testing.T, s string, keys openpgp.EntityList) (plaintext, comment string) {
	t.Helper()
	block, err := armor.Decode(strings.NewReader(s))
	if err != nil {
		t.Fatalf("not armored: %v\n%q", err, s)
	}
	md, err := openpgp.ReadMessage(block.Body, keys, nil, nil)
	if err != nil {
		t.Fatalf("cannot open: %v", err)
	}
	b, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), block.Header["Comment"]
}

type servedPart struct {
	contentType, filename, body string
}

// wholeMessageParts parses BODY[] of fetched, multipart, and returns its
// parts, transfer encodings undone.
func wholeMessageParts(t *testing.T, fetched *imap.Message) []servedPart {
	t.Helper()
	e, err := message.Read(bytes.NewReader(sectionBytes(t, fetched, "BODY.PEEK[]")))
	if err != nil {
		t.Fatalf("BODY[] doesn't parse: %v", err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("BODY[] isn't multipart")
	}
	var parts []servedPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return parts
		} else if err != nil {
			t.Fatal(err)
		}
		ct, _, _ := p.Header.ContentType()
		_, params, _ := p.Header.ContentDisposition()
		b, err := io.ReadAll(p.Body)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, servedPart{ct, params["filename"], string(b)})
	}
}

// A message encrypted end to end to a key Proton doesn't have, but with
// attachments, isn't passed through as PGP/MIME, which has no room for them.
// Nothing is lost: the body and the attachment no key opens are served still
// encrypted, armored, with a comment saying why, as bridge does; an attachment
// the account's key opens is served decrypted.
func TestEncryptedToOtherKeyWithAttachmentsKept(t *testing.T) {
	account, recipient := newKey(t, "account"), newKey(t, "recipient")
	msg := fetchTestMessage("a", 0)
	msg.Body = encryptBody(t, "secret body\r\n", recipient)
	foreign, foreignData := encryptAttachment(t, "att1", "a", "plan.txt", "secret attachment", recipient)
	own, ownData := encryptAttachment(t, "att2", "a", "notes.txt", "readable attachment", account)
	msg.Attachments = []*protonmail.Attachment{foreign, own}
	msg.NumAttachments = 2

	api := &fakeAPI{t: t, gets: map[string]fakeResponse{
		"/attachments/att1": {http.StatusOK, "application/octet-stream", foreignData},
		"/attachments/att2": {http.StatusOK, "application/octet-stream", ownData},
	}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	mbox.u.u.Keys = []*protonmail.PrivateKey{publicKey(t, "k1", account)}
	cacheFull(mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, "BODY.PEEK[]")

	bs := fetched.BodyStructure
	if bs.MIMEType != "multipart" || bs.MIMESubType != "mixed" || len(bs.Parts) != 3 {
		t.Fatalf("BODYSTRUCTURE %v/%v with %v parts, want multipart/mixed with 3", bs.MIMEType, bs.MIMESubType, len(bs.Parts))
	}
	if p := bs.Parts[0]; p.MIMEType+"/"+p.MIMESubType != "text/plain" || p.Encoding != "7bit" {
		t.Errorf("BODYSTRUCTURE part 1 %v/%v %v, want text/plain 7bit", p.MIMEType, p.MIMESubType, p.Encoding)
	}
	if p := bs.Parts[1]; p.MIMEType+"/"+p.MIMESubType != "application/octet-stream" || p.DispositionParams["filename"] != "plan.txt.pgp" {
		t.Errorf("BODYSTRUCTURE part 2 %v/%v %v, want application/octet-stream plan.txt.pgp", p.MIMEType, p.MIMESubType, p.DispositionParams)
	}
	if p := bs.Parts[2]; p.MIMEType+"/"+p.MIMESubType != "text/plain" || p.DispositionParams["filename"] != "notes.txt" {
		t.Errorf("BODYSTRUCTURE part 3 %v/%v %v, want text/plain notes.txt", p.MIMEType, p.MIMESubType, p.DispositionParams)
	}

	parts := wholeMessageParts(t, fetched)
	if len(parts) != 3 {
		t.Fatalf("BODY[] has %v parts, want 3", len(parts))
	}
	body, comment := readArmored(t, parts[0].body, openpgp.EntityList{recipient})
	if body != "secret body\r\n" || !strings.Contains(comment, "could not be decrypted") {
		t.Errorf("body part opens to %q with comment %q, want the body and why", body, comment)
	}
	if parts[1].filename != "plan.txt.pgp" {
		t.Errorf("first attachment named %q, want plan.txt.pgp", parts[1].filename)
	}
	data, comment := readArmored(t, parts[1].body, openpgp.EntityList{recipient})
	if data != "secret attachment" || !strings.Contains(comment, "could not be decrypted") {
		t.Errorf("first attachment opens to %q with comment %q, want its data and why", data, comment)
	}
	if parts[2].filename != "notes.txt" || parts[2].body != "readable attachment" {
		t.Errorf("second attachment %+v, want notes.txt decrypted", parts[2])
	}
}

// A body encrypted to a key of the account that peroxide didn't unlock (one
// added after it started, or that failed to unlock) isn't passed through: the
// user's own client couldn't open it. It is served armored, saying why, and
// the FETCH succeeds.
func TestEncryptedToLockedAccountKey(t *testing.T) {
	unlocked, locked := newKey(t, "unlocked"), newKey(t, "locked")
	msg := fetchTestMessage("a", 0)
	msg.Body = encryptBody(t, "to the new key\r\n", locked)
	api := &fakeAPI{t: t}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{unlocked}
	mbox.u.u.Keys = []*protonmail.PrivateKey{publicKey(t, "k1", unlocked)}
	mbox.u.addrs = []*protonmail.Address{{ID: "a1", Email: "bob@example.org", Keys: []*protonmail.PrivateKey{publicKey(t, "k2", locked)}}}
	cacheFull(mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, "BODY.PEEK[1]")
	if bs := fetched.BodyStructure; bs.MIMESubType == "encrypted" {
		t.Fatalf("passed through as PGP/MIME, want not")
	}
	text, comment := readArmored(t, string(sectionBytes(t, fetched, "BODY.PEEK[1]")), openpgp.EntityList{locked})
	if text != "to the new key\r\n" || !strings.Contains(comment, "hasn't unlocked") {
		t.Errorf("BODY[1] opens to %q with comment %q, want the body and that the key isn't unlocked", text, comment)
	}
}

// A message deleted in Proton but still listed (its delete event isn't
// applied yet) is left out of a FETCH, which still answers the others.
func TestFetchSkipsMessageDeletedInProton(t *testing.T) {
	gone, kept := fetchTestMessage("gone", 0), fetchTestMessage("kept", 0)
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{
		"/messages/gone": {http.StatusUnprocessableEntity, "application/json", `{"Code":2501,"Error":"Message does not exist"}`},
	}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{gone, kept})
	cacheFull(mbox, kept)

	msgs, err := fetchAll(t, mbox, imap.FetchBodyStructure)
	if err != nil {
		t.Fatalf("FETCH = %v, want the other message", err)
	}
	if len(msgs) != 1 || msgs[0].SeqNum != 2 {
		t.Errorf("FETCH sent %v messages, want message 2 only", len(msgs))
	}
}

// Any other failure to get a message still fails the FETCH: the client then
// retries, rather than taking a missing message for an expunged one.
func TestFetchFailsOnOtherErrors(t *testing.T) {
	msg := fetchTestMessage("a", 0)
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{
		"/messages/a": {http.StatusTooManyRequests, "application/json", `{"Code":2028,"Error":"Too many requests"}`},
	}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	if _, err := fetchAll(t, mbox, imap.FetchBodyStructure); err == nil {
		t.Error("FETCH succeeded, want the rate limit reported")
	}
}
