package protonmail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/emersion/go-message"
)

func newImportKey(t *testing.T) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("i", "", "i@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// decryptArmored returns the plaintext of an armored PGP message to key.
func decryptArmored(t *testing.T, key *openpgp.Entity, armored []byte) string {
	t.Helper()
	block, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		t.Fatalf("armor.Decode() = %v", err)
	}
	return decryptBinary(t, key, block.Body)
}

func decryptBinary(t *testing.T, key *openpgp.Entity, r io.Reader) string {
	t.Helper()
	md, err := openpgp.ReadMessage(r, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		t.Fatalf("openpgp.ReadMessage() = %v", err)
	}
	b, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatalf("read the plaintext: %v", err)
	}
	return string(b)
}

// A message with one text part and attachments keeps its MIME structure: the
// text is an armored PGP message, decoded from its transfer encoding and
// charset to UTF-8; each attachment a base64 binary PGP message of its bytes.
// evidence: witnesses
func TestEncryptRFC822PerPart(t *testing.T) {
	key := newImportKey(t)
	literal := "From: a@example.org\r\n" +
		"To: b@example.org\r\n" +
		"Subject: hi\r\n" +
		"Content-Type: multipart/mixed; boundary=b1\r\n" +
		"\r\n" +
		"--b1\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"caf=E9\r\n" +
		"--b1\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=f.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		base64.StdEncoding.EncodeToString([]byte("\x00\x01data")) + "\r\n" +
		"--b1--\r\n"

	enc, err := encryptRFC822(key, []byte(literal))
	if err != nil {
		t.Fatalf("encryptRFC822() = %v", err)
	}
	e, err := message.Read(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("message.Read() = %v", err)
	}
	if got := e.Header.Get("Subject"); got != "hi" {
		t.Errorf("Subject = %q, want hi", got)
	}
	if mt, params, _ := e.Header.ContentType(); mt != "multipart/mixed" || params["boundary"] != "b1" {
		t.Errorf("Content-Type = %v %v, want multipart/mixed boundary b1", mt, params)
	}
	mr := e.MultipartReader()
	text, err := mr.NextPart()
	if err != nil {
		t.Fatalf("first part: %v", err)
	}
	if _, params, _ := text.Header.ContentType(); params["charset"] != "utf-8" {
		t.Errorf("text charset = %q, want utf-8", params["charset"])
	}
	if text.Header.Has("Content-Transfer-Encoding") {
		t.Error("the text part kept its Content-Transfer-Encoding")
	}
	body, _ := io.ReadAll(text.Body)
	if got := decryptArmored(t, key, body); got != "caf\u00e9" {
		t.Errorf("text = %q, want caf\\u00e9 in UTF-8", got)
	}
	att, err := mr.NextPart()
	if err != nil {
		t.Fatalf("second part: %v", err)
	}
	if _, params, _ := att.Header.ContentDisposition(); params["filename"] != "f.bin" {
		t.Errorf("attachment filename = %q, want f.bin", params["filename"])
	}
	// go-message has decoded the base64.
	if got := decryptBinary(t, key, att.Body); got != "\x00\x01data" {
		t.Errorf("attachment = %q, want its bytes", got)
	}
	if _, err := mr.NextPart(); !errors.Is(err, io.EOF) {
		t.Errorf("a third part: %v", err)
	}
}

// Messages encryptRFC822 can't encrypt part by part are encrypted whole as
// PGP/MIME, signed, under a header keeping the fields Proton lists by; the
// same message gives the same boundary.
// evidence: witnesses
func TestEncryptRFC822Whole(t *testing.T) {
	key := newImportKey(t)
	head := "From: a@example.org\r\nTo: b@example.org\r\nSubject: s\r\nMessage-Id: <m@x>\r\nX-Other: o\r\n"
	for _, tt := range []struct {
		name, literal string
	}{
		{"signed", head + "Content-Type: multipart/signed; boundary=s; protocol=\"application/pgp-signature\"\r\n\r\n--s\r\n\r\nt\r\n--s--\r\n"},
		{"encrypted", head + "Content-Type: multipart/encrypted; boundary=s\r\n\r\n--s\r\n\r\nt\r\n--s--\r\n"},
		{"two texts", head + "Content-Type: multipart/alternative; boundary=s\r\n\r\n--s\r\nContent-Type: text/plain\r\n\r\nt\r\n--s\r\nContent-Type: text/html\r\n\r\n<p>t</p>\r\n--s--\r\n"},
		{"unknown charset", head + "Content-Type: text/plain; charset=x-nonesuch\r\n\r\nt\r\n"},
		{"bad disposition", head + "Content-Disposition: ;;\r\n\r\nt\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := encryptRFC822(key, []byte(tt.literal))
			if err != nil {
				t.Fatalf("encryptRFC822() = %v", err)
			}
			again, err := encryptRFC822(key, []byte(tt.literal))
			if err != nil {
				t.Fatalf("encryptRFC822() again = %v", err)
			}
			e, err := message.Read(bytes.NewReader(enc))
			if err != nil {
				t.Fatalf("message.Read() = %v", err)
			}
			mt, params, _ := e.Header.ContentType()
			if mt != "multipart/encrypted" || params["protocol"] != "application/pgp-encrypted" {
				t.Fatalf("Content-Type = %v %v, want PGP/MIME", mt, params)
			}
			e2, _ := message.Read(bytes.NewReader(again))
			if _, params2, _ := e2.Header.ContentType(); params2["boundary"] != params["boundary"] {
				t.Errorf("boundaries %q and %q for the same message", params["boundary"], params2["boundary"])
			}
			for k, want := range map[string]string{"From": "a@example.org", "Subject": "s", "Message-Id": "<m@x>", "X-Other": ""} {
				if got := e.Header.Get(k); got != want {
					t.Errorf("%v = %q, want %q", k, got, want)
				}
			}
			mr := e.MultipartReader()
			if _, err := mr.NextPart(); err != nil {
				t.Fatalf("control part: %v", err)
			}
			p, err := mr.NextPart()
			if err != nil {
				t.Fatalf("encrypted part: %v", err)
			}
			armored, _ := io.ReadAll(p.Body)
			block, err := armor.Decode(bytes.NewReader(armored))
			if err != nil {
				t.Fatalf("armor.Decode() = %v", err)
			}
			md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
			if err != nil {
				t.Fatalf("ReadMessage() = %v", err)
			}
			plain, _ := io.ReadAll(md.UnverifiedBody)
			if string(plain) != tt.literal {
				t.Errorf("plaintext = %q, want the message", plain)
			}
			if md.SignatureError != nil || md.Signature == nil {
				t.Errorf("signature %v, error %v: want signed by the key", md.Signature, md.SignatureError)
			}
		})
	}
}

// ImportMessage posts the encrypted message and its metadata, keyed by one
// name, and returns the message ID of that name's result; a failed result is
// its API error.
// evidence: witnesses
func TestImportMessage(t *testing.T) {
	key := newImportKey(t)
	literal := "From: a@example.org\r\nSubject: s\r\n\r\nbody\r\n"
	for _, tt := range []struct {
		name     string
		response string
		wantID   string
		wantCode int
	}{
		{"imported", `{"Code":1001,"Responses":[{"Name":"0","Response":{"Code":1000,"MessageID":"m1"}}]}`, "m1", 0},
		{"refused", `{"Code":1001,"Responses":[{"Name":"0","Response":{"Code":2024,"Error":"too big"}}]}`, "", 2024},
		{"no result", `{"Code":1001,"Responses":[]}`, "", -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var parts map[string][]byte
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.Method + " " + r.URL.Path
				parts = make(map[string][]byte)
				_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil {
					t.Error(err)
				}
				mr := multipart.NewReader(r.Body, params["boundary"])
				for {
					p, err := mr.NextPart()
					if errors.Is(err, io.EOF) {
						break
					} else if err != nil {
						t.Error(err)
						break
					}
					b, _ := io.ReadAll(p)
					parts[p.FormName()] = b
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tt.response)
			}))
			defer srv.Close()
			c := &Client{RootURL: srv.URL}

			meta := &ImportMetadata{AddressID: "a1", LabelIDs: []string{"0", "10"}, Unread: true, Flags: MessageFlagReceived | MessageFlagReplied}
			id, err := c.ImportMessage(t.Context(), key, meta, []byte(literal))
			if path != "POST /mail/v4/messages/import" {
				t.Errorf("request %q", path)
			}
			var gotMeta map[string]map[string]any
			if err := json.Unmarshal(parts["Metadata"], &gotMeta); err != nil {
				t.Fatalf("Metadata %q: %v", parts["Metadata"], err)
			}
			m := gotMeta["0"]
			if m["AddressID"] != "a1" || m["Unread"] != 1.0 || m["Flags"] != 33.0 || len(m["LabelIDs"].([]any)) != 2 {
				t.Errorf("Metadata = %v", gotMeta)
			}
			if !strings.Contains(string(parts["0"]), "-----BEGIN PGP MESSAGE-----") || !strings.HasPrefix(string(parts["0"]), "From: a@example.org") {
				t.Errorf("message part %q, want the header and an encrypted body", parts["0"])
			}
			if id != tt.wantID {
				t.Errorf("ImportMessage() = %q, want %q", id, tt.wantID)
			}
			switch {
			case tt.wantCode > 0:
				if apiErr, ok := errors.AsType[*APIError](err); !ok || apiErr.Code != tt.wantCode {
					t.Errorf("error = %v, want API code %v", err, tt.wantCode)
				}
			case tt.wantCode < 0:
				if err == nil {
					t.Error("no error for a response without the message")
				}
			case err != nil:
				t.Errorf("ImportMessage() error = %v", err)
			}
		})
	}
}

// A message over MaxImportSize once encrypted is refused before any request.
// evidence: witnesses
func TestImportMessageTooBig(t *testing.T) {
	key := newImportKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request %v %v", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := &Client{RootURL: srv.URL}

	// Encrypted, an attachment grows by a third (base64): 1 MiB over.
	att := make([]byte, MaxImportSize*3/4+(1<<20))
	literal := "From: a@example.org\r\nContent-Type: application/octet-stream\r\n\r\n" + string(att)
	_, err := c.ImportMessage(t.Context(), key, &ImportMetadata{}, []byte(literal))
	if !errors.Is(err, ErrImportSizeExceeded) {
		t.Errorf("ImportMessage() = %v, want ErrImportSizeExceeded", err)
	}
}
