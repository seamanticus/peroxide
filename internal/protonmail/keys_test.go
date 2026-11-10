package protonmail

import (
	"errors"
	"testing"
)

// The key mail is encrypted to is the first one Proton marks active; an
// address without keys takes plain mail; one whose keys are all obsolete is
// refused rather than sent in clear.
// evidence: witnesses
func TestSendingKey(t *testing.T) {
	obsolete := &PublicKey{Flags: PublicKeyTrusted, PublicKey: "old"}
	active := &PublicKey{Flags: PublicKeyTrusted | PublicKeyActive, PublicKey: "new"}
	compromised := &PublicKey{Flags: PublicKeyActive, PublicKey: "compromised"}
	for _, tc := range []struct {
		name    string
		keys    []*PublicKey
		want    *PublicKey
		wantErr error
	}{
		{"no keys", nil, nil, nil},
		{"first active", []*PublicKey{active, obsolete}, active, nil},
		{"obsolete skipped", []*PublicKey{obsolete, active}, active, nil},
		// Encrypting needs active only: compromised says its signatures
		// can't be trusted, not that it can't decrypt.
		{"compromised but active", []*PublicKey{compromised}, compromised, nil},
		{"none active", []*PublicKey{obsolete}, nil, ErrNoActiveKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&PublicKeyResp{Keys: tc.keys}).SendingKey()
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Errorf("SendingKey() = %v, %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
