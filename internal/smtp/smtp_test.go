package smtp

import (
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

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
