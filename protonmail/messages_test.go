package protonmail

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

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

// Card types are a bit set: every value the API sends decodes to the right
// protection.
// proof: exhaustive
func TestContactCardFlags(t *testing.T) {
	for typ := range 4 {
		var card ContactCard
		if err := json.Unmarshal(fmt.Appendf(nil, `{"Type":%d}`, typ), &card); err != nil {
			t.Fatalf("Type %v: %v", typ, err)
		}
		if got, want := card.Type.Encrypted(), typ&1 != 0; got != want {
			t.Errorf("Type %v: Encrypted() = %v, want %v", typ, got, want)
		}
		if got, want := card.Type.Signed(), typ&2 != 0; got != want {
			t.Errorf("Type %v: Signed() = %v, want %v", typ, got, want)
		}
	}
}
