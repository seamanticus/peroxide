package smtp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

func armored(t *testing.T, blockType string, data []byte) string {
	t.Helper()
	var b bytes.Buffer
	w, err := armor.Encode(&b, blockType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(b.String(), "\n", "\r\n")
}

// A multipart/signed message, laid out as aerc sends it, yields its first
// part's exact bytes, those the client's signature covers.
// evidence: witnesses
func TestParseSecuredSigned(t *testing.T) {
	signer, err := openpgp.NewEntity("c", "", "c@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	content := "Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Type: text/plain; charset=UTF-8; format=flowed\r\n\r\nThis is a signed message\r\n"
	var sig bytes.Buffer
	if err := openpgp.DetachSign(&sig, signer, strings.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	raw := "Mime-Version: 1.0\r\nContent-Type: multipart/signed;\r\n boundary=bb;\r\n" +
		" micalg=pgp-sha512; protocol=\"application/pgp-signature\"\r\n" +
		"From: <c@example.org>\r\nTo: <x@example.org>\r\n\r\n" +
		"--bb\r\n" + content + "\r\n--bb\r\n" +
		"Content-Type: application/pgp-signature; name=\"signature.asc\"\r\n\r\n" +
		armored(t, openpgp.SignatureType, sig.Bytes()) + "\r\n--bb--\r\n"

	sec, ok, err := parseSecured([]byte(raw))
	if err != nil || !ok || sec.signed == nil {
		t.Fatalf("parseSecured() = %+v, %v, %v; want signed", sec, ok, err)
	}
	if got := string(sec.signed.content); got != content {
		t.Errorf("signed content = %q, want %q", got, content)
	}
	if _, err := openpgp.CheckDetachedSignature(openpgp.EntityList{signer}, bytes.NewReader(sec.signed.content), &sig, nil); err != nil {
		t.Errorf("the client's signature doesn't verify over the split content: %v", err)
	}
}

// A multipart/encrypted message yields its key packets and its data
// packet, which together open to the client's plaintext; plain mail, and
// other multipart types, are not secured.
// evidence: witnesses
func TestParseSecuredEncrypted(t *testing.T) {
	rcpt, err := openpgp.NewEntity("r", "", "r@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	self, err := openpgp.NewEntity("s", "", "s@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var ct bytes.Buffer
	w, err := openpgp.Encrypt(&ct, openpgp.EntityList{rcpt, self}, self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	const plain = "Content-Type: text/plain\r\n\r\nsecret\r\n"
	if _, err := w.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw := "Mime-Version: 1.0\r\nContent-Type: multipart/encrypted;\r\n boundary=ee;\r\n" +
		" protocol=\"application/pgp-encrypted\"\r\nFrom: <s@example.org>\r\n\r\n" +
		"--ee\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"\r\n--ee\r\nContent-Type: application/octet-stream\r\n\r\n" +
		armored(t, "PGP MESSAGE", ct.Bytes()) + "\r\n--ee--\r\n"

	sec, ok, err := parseSecured([]byte(raw))
	if err != nil || !ok || sec.encrypted == nil {
		t.Fatalf("parseSecured() = %+v, %v, %v; want encrypted", sec, ok, err)
	}
	joined := append(append([]byte{}, sec.encrypted.keyPackets...), sec.encrypted.data...)
	if !bytes.Equal(joined, ct.Bytes()) {
		t.Error("key and data packets don't join back to the ciphertext")
	}
	for _, key := range []*openpgp.Entity{rcpt, self} {
		md, err := openpgp.ReadMessage(bytes.NewReader(joined), openpgp.EntityList{key}, nil, nil)
		if err != nil {
			t.Fatalf("%s can't open the joined packets: %v", key.PrimaryIdentity().Name, err)
		}
		var got bytes.Buffer
		if _, err := got.ReadFrom(md.UnverifiedBody); err != nil || got.String() != plain {
			t.Errorf("%s reads %q, %v; want %q", key.PrimaryIdentity().Name, got.String(), err, plain)
		}
	}

	for _, other := range []string{
		"From: a@example.org\r\nContent-Type: text/plain\r\n\r\nhi\r\n",
		"From: a@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nhi\r\n--x--\r\n",
		"From: a@example.org\r\nContent-Type: multipart/signed; boundary=x; protocol=\"application/pkcs7-signature\"\r\n\r\n--x--\r\n",
	} {
		if sec, ok, err := parseSecured([]byte(other)); ok || err != nil {
			t.Errorf("parseSecured(%q) = %+v, %v, %v; want not secured", other, sec, ok, err)
		}
	}
	broken := "Content-Type: multipart/encrypted; boundary=ee; protocol=\"application/pgp-encrypted\"\r\n\r\n--ee--\r\n"
	if _, _, err := parseSecured([]byte(broken)); err == nil {
		t.Error("parseSecured() of an encrypted message without ciphertext succeeded")
	}
}

// The message sent signed to recipients without a key is the client's, but
// for its Bcc field, which would show who was hidden.
// evidence: witnesses
func TestMIMEBodyDropsBcc(t *testing.T) {
	raw := "From: a@example.org\r\nTo: b@example.org\r\nBcc: hidden@example.org\r\nSubject: s\r\n" +
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nbody\r\n--x--\r\n"
	got, err := mimeBody([]byte(raw))
	if err != nil {
		t.Fatalf("mimeBody() = %v", err)
	}
	if strings.Contains(string(got), "hidden@example.org") {
		t.Errorf("mimeBody() kept the Bcc:\n%s", got)
	}
	for _, want := range []string{"To: b@example.org", "Subject: s", "\r\n\r\n--x\r\n\r\nbody\r\n--x--\r\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("mimeBody() lost %q:\n%s", want, got)
		}
	}
}

// With "Sign external messages" on, every recipient without a key is in one
// signed MIME package, multipart/mixed with no attachment keys; off, they
// get the plain package, unsigned.
// evidence: witnesses
func TestKeylessPackages(t *testing.T) {
	signer, err := openpgp.NewEntity("s", "", "s@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: s@example.org\r\nContent-Type: text/plain\r\n\r\nhi\r\n")
	rcpts := []string{"x@example.org", "y@example.org"}

	signed, err := signedMIMEPackage(raw, signer, rcpts)
	if err != nil {
		t.Fatalf("signedMIMEPackage() = %v", err)
	}
	if signed.Type != protonmail.MessagePackageMIME || signed.MIMEType != "multipart/mixed" || len(signed.AttachmentKeys) != 0 {
		t.Errorf("signed package type %v %q, %v attachment keys; want ClearMIME multipart/mixed, none", signed.Type, signed.MIMEType, len(signed.AttachmentKeys))
	}
	for _, r := range rcpts {
		if p := signed.Addresses[r]; p == nil || p.Signature != 1 {
			t.Errorf("signed package for %v = %+v, want signed", r, p)
		}
	}

	plain, err := cleartextPackage([]byte("hi"), "text/plain", nil, signer, rcpts)
	if err != nil {
		t.Fatalf("cleartextPackage() = %v", err)
	}
	for _, r := range rcpts {
		if p := plain.Addresses[r]; p == nil || p.Signature != 0 || p.Type != protonmail.MessagePackageCleartext {
			t.Errorf("plain package for %v = %+v, want cleartext, unsigned", r, p)
		}
	}
}
