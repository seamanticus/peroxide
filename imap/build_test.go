package imap

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/emersion/go-imap"

	"github.com/seamanticus/peroxide/protonmail"
)

// encryptSignedBody returns plaintext encrypted to to and signed by signer,
// armored as Proton stores a message received as PGP/MIME.
func encryptSignedBody(t *testing.T, plaintext string, to, signer *openpgp.Entity) string {
	t.Helper()
	var b bytes.Buffer
	aw, err := armor.Encode(&b, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, []*openpgp.Entity{to}, signer, nil, nil)
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

// A message received as PGP/MIME (MIMEType multipart/mixed, the decrypted
// body a whole MIME entity) is that entity under the message's header, as
// bridge builds it: its structure is the entity's, and RFC822.SIZE is the
// length of BODY[]. Today's single part without a boundary is gone.
// evidence: witnesses
func TestPGPMIMEMessageIsItsEntity(t *testing.T) {
	account := newKey(t, "account")
	entity := "Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<p>html</p>\r\n--b--\r\n"
	msg := fetchTestMessage("a", 0)
	msg.MIMEType = "multipart/mixed"
	msg.IsEncrypted = protonmail.MessageEncryptedPGPMIME
	msg.Body = encryptBody(t, entity, account)
	mbox, _ := newActionTestMailbox(t, &fakeAPI{t: t}, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	cacheFull(t, mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, imap.FetchRFC822Size, "BODY.PEEK[]", "BODY.PEEK[2]")
	bs := fetched.BodyStructure
	if bs.MIMESubType != "alternative" || len(bs.Parts) != 2 || bs.Parts[1].MIMESubType != "html" {
		t.Errorf("BODYSTRUCTURE %v/%v with %v parts, want the entity's multipart/alternative", bs.MIMEType, bs.MIMESubType, len(bs.Parts))
	}
	if got := string(sectionBytes(t, fetched, "BODY.PEEK[2]")); got != "<p>html</p>" {
		t.Errorf("BODY[2] = %q, want the html part", got)
	}
	whole := sectionBytes(t, fetched, "BODY.PEEK[]")
	if fetched.Size != uint32(len(whole)) {
		t.Errorf("RFC822.SIZE = %v, want %v, the length of BODY[]", fetched.Size, len(whole))
	}
	if n := strings.Count(string(whole), "Content-Type:"); n != 3 {
		t.Errorf("BODY[] has %v Content-Type fields, want 3 (the entity's and its parts')\n%s", n, whole)
	}
}

// A PGP/MIME message signed by its sender is served multipart/signed (RFC
// 3156 section 5), the entity byte for byte as signed and the signature as
// second part, so that the client can verify it.
// evidence: witnesses
func TestPGPMIMESignedMessage(t *testing.T) {
	account, sender := newKey(t, "account"), newKey(t, "sender")
	entity := "Content-Type: text/plain\r\n\r\nsigned words\r\n"
	msg := fetchTestMessage("a", 0)
	msg.MIMEType = "multipart/mixed"
	msg.Body = encryptSignedBody(t, entity, account, sender)
	mbox, _ := newActionTestMailbox(t, &fakeAPI{t: t}, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	cacheFull(t, mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, "BODY.PEEK[1]", "BODY.PEEK[1.MIME]", "BODY.PEEK[2]")
	bs := fetched.BodyStructure
	if bs.MIMESubType != "signed" || bs.Params["protocol"] != "application/pgp-signature" || !strings.HasPrefix(bs.Params["micalg"], "pgp-sha") {
		t.Errorf("BODYSTRUCTURE %v/%v %v, want multipart/signed with protocol and micalg", bs.MIMEType, bs.MIMESubType, bs.Params)
	}
	signed := append(sectionBytes(t, fetched, "BODY.PEEK[1.MIME]"), sectionBytes(t, fetched, "BODY.PEEK[1]")...)
	sig := sectionBytes(t, fetched, "BODY.PEEK[2]")
	block, err := armor.Decode(bytes.NewReader(sig))
	if err != nil {
		t.Fatalf("BODY[2] isn't an armored signature: %v", err)
	}
	if _, err := openpgp.CheckDetachedSignature(openpgp.EntityList{sender}, bytes.NewReader(signed), block.Body, nil); err != nil {
		t.Errorf("the sender's signature doesn't verify over BODY[1.MIME] BODY[1]: %v\n%q", err, signed)
	}
}

// An attached message is served without transfer encoding, as bridge does, so
// that its BODYSTRUCTURE carries its envelope and structure (RFC 3501 section
// 7.4.2), which a base64 part can't. go-imap's backendutil doesn't descend
// into an attached message, so BODY[2.1] is empty; BODY[2] is the message.
// evidence: witnesses
func TestAttachedMessageStructure(t *testing.T) {
	account := newKey(t, "account")
	inner := "Subject: inner\r\nFrom: a@example.org\r\nContent-Type: text/plain\r\n\r\ninner body\r\n"
	att, data := encryptAttachment(t, "att1", "a", "fwd.eml", inner, account)
	att.MIMEType = "message/rfc822"
	msg := fetchTestMessage("a", 0)
	msg.Body = encryptBody(t, "see attached\r\n", account)
	msg.Attachments = []*protonmail.Attachment{att}
	msg.NumAttachments = 1
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{"/attachments/att1": {http.StatusOK, "application/octet-stream", data}}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	cacheFull(t, mbox, msg)

	fetched := fetchItems(t, mbox, imap.FetchBodyStructure, "BODY.PEEK[2]")
	part := fetched.BodyStructure.Parts[1]
	if part.MIMEType+"/"+part.MIMESubType != "message/rfc822" || part.Encoding != "" {
		t.Fatalf("attachment %v/%v encoded %q, want message/rfc822 unencoded", part.MIMEType, part.MIMESubType, part.Encoding)
	}
	if part.Envelope == nil || part.Envelope.Subject != "inner" || part.BodyStructure == nil || part.BodyStructure.MIMESubType != "plain" {
		t.Errorf("attachment envelope %+v, structure %+v; want the inner message's", part.Envelope, part.BodyStructure)
	}
	if got := string(sectionBytes(t, fetched, "BODY.PEEK[2]")); got != inner {
		t.Errorf("BODY[2] = %q, want the inner message as it was", got)
	}
}

// An attachment whose key packets open but whose data fails to decrypt is
// served still encrypted, as <name>.pgp, as bridge does, instead of failing
// the FETCH.
// evidence: witnesses
func TestAttachmentDecryptFailureArmored(t *testing.T) {
	account := newKey(t, "account")
	att, data := encryptAttachment(t, "att1", "a", "plan.txt", strings.Repeat("secret ", 100), account)
	corrupt := []byte(data)
	corrupt[len(corrupt)-5] ^= 0xff
	msg := fetchTestMessage("a", 0)
	msg.Body = encryptBody(t, "body\r\n", account)
	msg.Attachments = []*protonmail.Attachment{att}
	msg.NumAttachments = 1
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{"/attachments/att1": {http.StatusOK, "application/octet-stream", string(corrupt)}}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	mbox.u.privateKeys = openpgp.EntityList{account}
	cacheFull(t, mbox, msg)

	parts := wholeMessageParts(t, fetchItems(t, mbox, "BODY.PEEK[]"))
	if len(parts) != 2 || parts[1].filename != "plan.txt.pgp" {
		t.Fatalf("parts %+v, want the body and plan.txt.pgp", parts)
	}
	block, err := armor.Decode(strings.NewReader(parts[1].body))
	if err != nil || !strings.Contains(block.Header["Comment"], "could not be decrypted") {
		t.Errorf("plan.txt.pgp: armor %v, comment %q; want armored with why", err, block.Header["Comment"])
	}
}

// Bytes go-imap can't parse are served as a note with them attached, so the
// client still gets them; the note is itself a message go-imap can parse.
// evidence: witnesses
func TestSafeMessage(t *testing.T) {
	msg := fetchTestMessage("a", 0)
	broken := []byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nnot a header line\r\n")
	literal, err := safeMessage(msg, broken, io.ErrUnexpectedEOF)
	if err != nil {
		t.Fatalf("safeMessage() = %v", err)
	}
	info, err := describe(literal)
	if err != nil {
		t.Fatalf("describe(safeMessage()) = %v", err)
	}
	bs := info.BodyStructure
	if len(bs.Parts) != 2 || bs.Parts[1].DispositionParams["filename"] != "message.eml" {
		t.Errorf("BODYSTRUCTURE %+v, want a note and message.eml", bs)
	}
}

// A section the message doesn't have is an empty string, as Dovecot and gluon
// answer, and the other items of the FETCH are served.
// evidence: witnesses
func TestFetchMissingSectionEmpty(t *testing.T) {
	msg := fetchTestMessage("a", 0)
	mbox, _ := newActionTestMailbox(t, &fakeAPI{t: t}, []*protonmail.Message{msg})
	cacheFull(t, mbox, msg)

	fetched := fetchItems(t, mbox, "BODY.PEEK[1]", "BODY.PEEK[3]", "BODY.PEEK[1.2]")
	if got := sectionBytes(t, fetched, "BODY.PEEK[3]"); len(got) != 0 {
		t.Errorf("BODY[3] = %q, want empty", got)
	}
	if got := sectionBytes(t, fetched, "BODY.PEEK[1.2]"); len(got) != 0 {
		t.Errorf("BODY[1.2] = %q, want empty", got)
	}
	if got := string(sectionBytes(t, fetched, "BODY.PEEK[1]")); !strings.Contains(got, "Hello") {
		t.Errorf("BODY[1] = %q, want the body", got)
	}
}

// FETCH BODY carries no extension data at any level (RFC 3501 section
// 7.4.2), BODYSTRUCTURE does; both describe the same parts.
// evidence: witnesses
func TestFetchBodyWithoutExtension(t *testing.T) {
	msg := fetchTestMessage("a", 0)
	mbox, _ := newActionTestMailbox(t, &fakeAPI{t: t}, []*protonmail.Message{msg})
	cacheFull(t, mbox, msg)

	rec, err := mbox.u.db.BodyRecord("a")
	if err != nil {
		t.Fatal(err)
	}
	ext := rec.BodyStructure
	plain := fetchItems(t, mbox, imap.FetchBody).BodyStructure
	if plain.Extended || plain.Parts[0].Extended {
		t.Error("FETCH BODY has extension data")
	}
	if !ext.Parts[0].Extended {
		t.Error("the stored BODYSTRUCTURE has no extension data")
	}
	if len(plain.Parts) != len(ext.Parts) || plain.Parts[0].Size != ext.Parts[0].Size {
		t.Errorf("BODY and BODYSTRUCTURE describe different parts: %+v, %+v", plain, ext)
	}
}
