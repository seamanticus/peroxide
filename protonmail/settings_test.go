package protonmail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// A signed MIME package carries the body key in clear (ClearMIME, 32), signed
// recipients and no attachment keys, and its body opens with that key to the
// MIME message, signed by the sender. One not multipart/mixed, or not
// signed, is refused, as go-proton-api's newMIMEPackage refuses them.
// evidence: witnesses
func TestAddClearMIME(t *testing.T) {
	signer, err := openpgp.NewEntity("s", "", "s@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	entity := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nhi\r\n--b--\r\n"
	encrypt := func(mimeType string, by *openpgp.Entity) *MessagePackageSet {
		set := NewMessagePackageSet(nil)
		w, err := set.Encrypt(mimeType, by)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, entity)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return set
	}

	set := encrypt("multipart/mixed", signer)
	pkg, err := set.AddClearMIME("x@example.org")
	if err != nil {
		t.Fatalf("AddClearMIME() = %v", err)
	}
	if pkg.Type != MessagePackageMIME || pkg.Signature != 1 || pkg.BodyKeyPacket != "" || pkg.AttachmentKeyPackets != nil {
		t.Errorf("package %+v, want ClearMIME, signed, no key packets", pkg)
	}
	if set.Type != MessagePackageMIME || set.BodyKey == nil || len(set.AttachmentKeys) != 0 {
		t.Errorf("set type %v, body key %v, attachment keys %v; want ClearMIME with a clear body key", set.Type, set.BodyKey, set.AttachmentKeys)
	}

	// The body is a bare data packet, as Proton expects: given a key packet
	// carrying the clear session key to a test key, it opens to the entity,
	// the sender's signature verifying.
	key, err := base64.StdEncoding.DecodeString(set.BodyKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(set.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := openpgp.NewEntity("r", "", "r@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var msg bytes.Buffer
	readerKey := reader.Subkeys[0].PublicKey
	if err := packet.SerializeEncryptedKey(&msg, readerKey, packet.CipherAES256, key, nil); err != nil {
		t.Fatal(err)
	}
	msg.Write(data)
	md, err := openpgp.ReadMessage(&msg, openpgp.EntityList{reader, signer}, nil, nil)
	if err != nil {
		t.Fatalf("the body doesn't open with its clear key: %v", err)
	}
	body, err := io.ReadAll(md.UnverifiedBody)
	if err != nil || string(body) != entity || md.SignatureError != nil || md.SignedBy == nil {
		t.Errorf("body %q, %v; signature %v by %v; want the entity, signed", body, err, md.SignatureError, md.SignedBy)
	}

	b, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"MIMEType":"multipart/mixed"`) || !strings.Contains(string(b), `"Type":32`) {
		t.Errorf("JSON %s, want MIMEType multipart/mixed and Type 32", b)
	}

	if _, err := encrypt("text/plain", signer).AddClearMIME("x@example.org"); err == nil {
		t.Error("AddClearMIME() of a text/plain body succeeded")
	}
	if _, err := encrypt("multipart/mixed", nil).AddClearMIME("x@example.org"); err == nil {
		t.Error("AddClearMIME() of an unsigned body succeeded")
	}
}

// GetMailSettings reads "Sign external messages".
// evidence: witnesses
func TestGetMailSettings(t *testing.T) {
	for _, tc := range []struct {
		body string
		want SignExternal
	}{
		{`{"Code":1000,"MailSettings":{"Sign":1,"PGPScheme":16}}`, SignExternalEnabled},
		{`{"Code":1000,"MailSettings":{"Sign":0}}`, SignExternalDisabled},
		{`{"Code":1000}`, SignExternalDisabled},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mail/v4/settings" {
				t.Errorf("request to %v", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, tc.body)
		}))
		s, err := (&Client{RootURL: srv.URL}).GetMailSettings(t.Context())
		srv.Close()
		if err != nil || s.Sign != tc.want {
			t.Errorf("GetMailSettings() of %s = %+v, %v; want Sign %v", tc.body, s, err, tc.want)
		}
	}
}
