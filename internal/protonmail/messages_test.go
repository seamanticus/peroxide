package protonmail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// An attachment signed with Sign is uploaded with its detached signature as
// the Signature part, binary, as go-proton-api's UploadAttachment sends it;
// one not signed has no such part.
// evidence: witnesses
func TestCreateAttachmentSendsSignature(t *testing.T) {
	signer, err := openpgp.NewEntity("s", "", "s@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = make(map[string][]byte)
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			} else if err != nil {
				t.Error(err)
				break
			}
			b, _ := io.ReadAll(p)
			got[p.FormName()] = b
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"Code":1000,"Attachment":{"ID":"att"}}`)
	}))
	defer srv.Close()
	c := &Client{RootURL: srv.URL}

	for _, signed := range []bool{true, false} {
		att := &Attachment{Name: "f.txt", MessageID: "m", MIMEType: "text/plain"}
		var raw []byte
		if signed {
			if raw, err = att.Sign(signer, strings.NewReader("file")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.CreateAttachment(att, strings.NewReader("data")); err != nil {
			t.Fatalf("CreateAttachment() = %v", err)
		}
		sig, ok := got["Signature"]
		if signed && (!ok || !bytes.Equal(sig, raw)) {
			t.Errorf("Signature part %x (sent %v), want %x", sig, ok, raw)
		}
		if !signed && ok {
			t.Error("an unsigned attachment was sent with a Signature part")
		}
		if signed {
			if _, err := openpgp.CheckDetachedSignature(openpgp.EntityList{signer}, strings.NewReader("file"), bytes.NewReader(sig), nil); err != nil {
				t.Errorf("the uploaded signature doesn't verify over the file: %v", err)
			}
		}
	}
}

// A message set whose keys use a cipher the API doesn't accept is reported,
// not a panic, and left unchanged.
// evidence: witnesses
func TestAddCleartextRejectsCipher(t *testing.T) {
	key := []byte("k")
	tests := []struct {
		name    string
		body    packet.CipherFunction
		att     packet.CipherFunction
		wantErr bool
	}{
		{"aes", packet.CipherAES256, packet.CipherAES128, false},
		{"3des body", packet.Cipher3DES, packet.CipherAES256, true},
		{"cast5 attachment", packet.CipherAES256, packet.CipherCAST5, true},
		{"unknown body", packet.CipherFunction(0), packet.CipherAES256, true},
	}
	for _, tt := range tests {
		set := NewMessagePackageSet(map[string]*packet.EncryptedKey{
			"att": {CipherFunc: tt.att, Key: key},
		})
		set.bodyKey = &packet.EncryptedKey{CipherFunc: tt.body, Key: key}

		pkg, err := set.AddCleartext("to@example.org")
		if gotErr := err != nil; gotErr != tt.wantErr {
			t.Errorf("%v: AddCleartext() error = %v, want error %v", tt.name, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			if pkg != nil || len(set.Addresses) != 0 || set.Type != 0 || set.BodyKey != nil || set.AttachmentKeys != nil {
				t.Errorf("%v: set changed by a failed AddCleartext: %+v", tt.name, set)
			}
			continue
		}
		if set.BodyKey == nil || set.AttachmentKeys["att"] == nil || set.Addresses["to@example.org"] != pkg {
			t.Errorf("%v: AddCleartext() left set %+v", tt.name, set)
		}
	}
}
