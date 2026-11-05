package imap

import (
	"bytes"
	"io"
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

func sectionBytes(t *testing.T, fetched *imap.Message, item imap.FetchItem) []byte {
	t.Helper()
	section, err := imap.ParseBodySectionName(item)
	if err != nil {
		t.Fatal(err)
	}
	l := fetched.GetBody(section)
	if l == nil {
		t.Fatalf("no %v in the response", item)
	}
	b, err := io.ReadAll(l)
	if err != nil {
		t.Fatal(err)
	}
	return b
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
