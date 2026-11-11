package auth

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/nacl/secretbox"

	"github.com/seamanticus/peroxide/internal/paths"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// The store key is HKDF-SHA256 of the secret key, salted with the user ID:
// the same inputs give the same key in every process, a different account
// or secret a different key, and it is never the secret key itself, which
// seals auth.json. An empty user ID is refused rather than giving every
// account the same salt.
// evidence: witnesses
func TestDeriveStoreKey(t *testing.T) {
	var secret, otherSecret [32]byte
	for i := range secret {
		secret[i] = byte(i)
		otherSecret[i] = byte(255 - i)
	}

	k, err := deriveStoreKey(&secret, "user-a")
	if err != nil {
		t.Fatalf("deriveStoreKey() = %v", err)
	}
	want, err := hkdf.Key(sha256.New, secret[:], []byte("user-a"), "peroxide message store v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k[:], want) {
		t.Errorf("deriveStoreKey() = %x, want %x (RFC 5869 with the documented salt and info)", k, want)
	}
	if again, _ := deriveStoreKey(&secret, "user-a"); again != k {
		t.Error("deriveStoreKey() differs between two calls")
	}
	if k == StoreKey(secret) {
		t.Error("store key = the secret key sealing auth.json")
	}
	if other, _ := deriveStoreKey(&secret, "user-b"); other == k {
		t.Error("two accounts got the same store key")
	}
	if other, _ := deriveStoreKey(&otherSecret, "user-a"); other == k {
		t.Error("two bridge passwords gave the same store key")
	}
	if _, err := deriveStoreKey(&secret, ""); err == nil {
		t.Error("deriveStoreKey() with no user ID succeeded")
	}
}

// auth.json holds the sealed credentials and the usernames in clear: only
// its owner may read it, whether saveAuths creates it or rewrites one an
// older peroxide left readable by everyone, whatever the umask.
// evidence: witnesses
func TestSaveAuthsOwnerOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing os.FileMode // 0: no file yet
	}{
		{"new file", 0},
		{"existing 0644", 0o644},
		{"existing 0666", 0o666},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// os.UserConfigDir reads XDG_CONFIG_HOME, else HOME: point both
			// away from the real ones.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			p, err := authFilePath()
			if err != nil {
				t.Fatal(err)
			}
			if tc.existing != 0 {
				if err := os.WriteFile(p, []byte("{}"), tc.existing); err != nil {
					t.Fatal(err)
				}
				// WriteFile's mode is masked by the umask: set it exactly.
				if err := os.Chmod(p, tc.existing); err != nil {
					t.Fatal(err)
				}
			}

			if err := saveAuths(map[string]string{"bob": "sealed"}); err != nil {
				t.Fatalf("saveAuths() = %v", err)
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != authFileMode {
				t.Errorf("%v mode = %v, want %v", filepath.Base(p), got, os.FileMode(authFileMode))
			}
			auths, err := readCachedAuths()
			if err != nil || auths["bob"] != "sealed" {
				t.Errorf("readCachedAuths() = %v, %v, want the saved record", auths, err)
			}
			// Written to a temporary file renamed over auth.json: none is
			// left beside it.
			entries, err := os.ReadDir(filepath.Dir(p))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(p) {
				t.Errorf("config directory holds %v, want only %v", entries, filepath.Base(p))
			}
		})
	}
}

// Accounts saved at the same time each read, change and rewrite auth.json:
// under auth.lock none loses another's record. Without the lock the loss
// depends on scheduling, so a pass is evidence only, stronger repeated.
// evidence: witnesses
func TestEncryptAndSaveConcurrent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	secretKey, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}

	const accounts = 8
	var wg sync.WaitGroup
	errs := make([]error, accounts)
	for i := range accounts {
		wg.Go(func() {
			errs[i] = EncryptAndSave(&CachedAuth{UID: fmt.Sprint("u", i)}, fmt.Sprint("user", i), secretKey)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("EncryptAndSave() = %v", err)
	}

	auths, err := readCachedAuths()
	if err != nil {
		t.Fatal(err)
	}
	for i := range accounts {
		if _, ok := auths[fmt.Sprint("user", i)]; !ok {
			t.Errorf("auth.json has no user%v: %v of %v accounts kept", i, len(auths), accounts)
		}
	}
}

// A running session's renewal goes to session.json, sealed with its session
// key, and never touches auth.json; it is saved only while auth.json holds
// the record the session was read from. Removing the account, or logging in
// anew (even with the same secret key: the record is sealed afresh), drops
// the saved session and refuses later saves.
// evidence: witnesses
func TestSaveSessionOwnRecordOnly(t *testing.T) {
	secret, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		change  func() error // after the session was read and saved once
		wantErr error
	}{
		{"own record", func() error { return nil }, nil},
		{"removed", func() error { return DeleteUsername("bob") }, errSessionNotStored},
		{"logged in anew", func() error { return EncryptAndSave(&CachedAuth{}, "bob", other) }, errSessionNotStored},
		{"logged in anew, same key", func() error { return EncryptAndSave(&CachedAuth{}, "bob", secret) }, errSessionNotStored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			if err := EncryptAndSave(&CachedAuth{}, "alice", other); err != nil {
				t.Fatal(err)
			}
			if err := EncryptAndSave(&CachedAuth{LoginPassword: "pw"}, "bob", secret); err != nil {
				t.Fatal(err)
			}
			_, id, err := readRecord("bob", secret)
			if err != nil {
				t.Fatal(err)
			}
			sessionKey, err := deriveSessionKey(secret, "bob")
			if err != nil {
				t.Fatal(err)
			}
			if sessionKey == *secret {
				t.Fatal("the session key is the secret key")
			}
			if err := saveSession(&protonmail.Auth{UID: "first"}, "bob", &sessionKey, id); err != nil {
				t.Fatalf("saveSession() = %v", err)
			}
			if got := mustStoredSession(t, "bob", &sessionKey); got == nil || got.UID != "first" {
				t.Fatalf("storedSession() = %+v, want the one saved", got)
			}
			if got := mustStoredSession(t, "bob", other); got != nil {
				t.Errorf("storedSession() with another key = %+v, want none", got)
			}

			if err := tc.change(); err != nil {
				t.Fatal(err)
			}
			p, err := authFilePath()
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != nil {
				if got := mustStoredSession(t, "bob", &sessionKey); got != nil {
					t.Errorf("after the change, storedSession() = %+v, want it dropped", got)
				}
			}

			if err := saveSession(&protonmail.Auth{UID: "renewed"}, "bob", &sessionKey, id); !errors.Is(err, tc.wantErr) {
				t.Fatalf("saveSession() = %v, want %v", err, tc.wantErr)
			}
			got := mustStoredSession(t, "bob", &sessionKey)
			if tc.wantErr == nil && (got == nil || got.UID != "renewed") {
				t.Errorf("storedSession() = %+v, want the renewed one", got)
			}
			if tc.wantErr != nil && got != nil {
				t.Errorf("storedSession() = %+v, want none: the save was refused", got)
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("saveSession() changed auth.json")
			}
		})
	}
}

// mustStoredSession is storedSession, failing the test on its error.
func mustStoredSession(t *testing.T, username string, sessionKey *[32]byte) *protonmail.Auth {
	t.Helper()
	stored, err := storedSession(username, sessionKey)
	if err != nil {
		t.Fatalf("storedSession() = %v", err)
	}
	return stored
}

// A sealed record too short to hold a nonce and a tag, as a truncated file
// gives, is an error, not a panic; so is one that isn't base64 or whose tag
// fails. A record that opens gives back what was sealed.
// evidence: witnesses
func TestDecryptRejectsDamagedRecords(t *testing.T) {
	key, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := encrypt([]byte(`{"a":1}`), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(raw)
	flipped[len(flipped)-1] ^= 1
	for _, tc := range []struct {
		name   string
		sealed string
	}{
		{"empty", ""},
		{"one byte", base64.StdEncoding.EncodeToString([]byte{1})},
		{"nonce only", base64.StdEncoding.EncodeToString(raw[:24])},
		{"nonce and part of the tag", base64.StdEncoding.EncodeToString(raw[:24+secretbox.Overhead-1])},
		{"not base64", "%%%"},
		{"tag fails", base64.StdEncoding.EncodeToString(flipped)},
	} {
		if _, err := decrypt(tc.sealed, key); err == nil {
			t.Errorf("%v: decrypt() = nil, want an error", tc.name)
		}
	}
	got, err := decrypt(sealed, key)
	if err != nil || strings.TrimRight(string(got), " ") != `{"a":1}` {
		t.Errorf("decrypt() = %q, %v, want the sealed text", got, err)
	}
}

// Sealed records are padded to whole blocks: passwords of different lengths
// within a block give records of one length, and the padding doesn't change
// what is read back.
// evidence: witnesses
func TestRecordsArePadded(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	key, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, padTo - 1, padTo, padTo + 1} {
		if got := len(pad(make([]byte, n))); got%padTo != 0 || got <= n {
			t.Errorf("len(pad(%v bytes)) = %v, want a multiple of %v above %v", n, got, padTo, n)
		}
	}
	lengths := map[int]bool{}
	for _, pw := range []string{"", "a", "a much longer login password than the others"} {
		if err := EncryptAndSave(&CachedAuth{LoginPassword: pw, MailboxPassword: pw}, "bob", key); err != nil {
			t.Fatal(err)
		}
		auths, err := readCachedAuths()
		if err != nil {
			t.Fatal(err)
		}
		lengths[len(auths["bob"])] = true
		record, _, err := readRecord("bob", key)
		if err != nil || record.LoginPassword != pw {
			t.Errorf("readRecord() = %+v, %v, want login password %q", record, err, pw)
		}
	}
	if len(lengths) != 1 {
		t.Errorf("records of %v lengths, want one", len(lengths))
	}
}

// storedSession tells no saved session (nil) from a session.json it can't
// read or a record its key opens that isn't a session (an error). A record
// of another bridge password is no session, not an error.
// evidence: witnesses
func TestStoredSessionErrors(t *testing.T) {
	key, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	notSession, err := encrypt([]byte(`[1]`), key)
	if err != nil {
		t.Fatal(err)
	}
	ofOther, err := encrypt([]byte(`{"UID":"u"}`), other)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		file    string // "": no file
		wantErr bool
	}{
		{"no file", "", false},
		{"no record", `{}`, false},
		{"another key's", `{"bob":"` + ofOther + `"}`, false},
		{"not JSON", `{"bob":`, true},
		{"not a session", `{"bob":"` + notSession + `"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			if tc.file != "" {
				p, err := paths.Path(sessionFile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := storedSession("bob", key)
			if (err != nil) != tc.wantErr || got != nil {
				t.Errorf("storedSession() = %+v, %v; want nil and an error: %v", got, err, tc.wantErr)
			}
		})
	}
}
