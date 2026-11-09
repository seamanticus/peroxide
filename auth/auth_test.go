package auth

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
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
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	for _, tc := range []struct {
		name     string
		existing os.FileMode // 0: no file yet
	}{
		{"new file", 0},
		{"existing 0644", 0o644},
		{"existing 0666", 0o666},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// os.UserConfigDir reads XDG_CONFIG_HOME on Unix, and HOME on
			// macOS: point both away from the real ones.
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

// A running session's renewal is saved over its own record only: once the
// account is removed, or logged in anew with another bridge password, the
// renewal leaves auth.json as it is.
// evidence: witnesses
func TestSaveSessionOwnRecordOnly(t *testing.T) {
	session, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		stored  *[32]byte // the key bob's record is sealed with; nil: no record
		wantErr error
		wantUID string // bob's UID in auth.json afterwards, "" for none
	}{
		{"own record", session, nil, "renewed"},
		{"removed", nil, errSessionNotStored, ""},
		{"logged in anew", other, errSessionNotStored, "anew"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			if err := EncryptAndSave(&CachedAuth{UID: "alice"}, "alice", other); err != nil {
				t.Fatal(err)
			}
			if tc.stored != nil {
				if err := EncryptAndSave(&CachedAuth{UID: "anew"}, "bob", tc.stored); err != nil {
					t.Fatal(err)
				}
			}

			if err := saveSession(&CachedAuth{UID: "renewed"}, "bob", session); !errors.Is(err, tc.wantErr) {
				t.Fatalf("saveSession() = %v, want %v", err, tc.wantErr)
			}

			auths, err := readCachedAuths()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := auths["alice"]; !ok {
				t.Error("alice's record is gone")
			}
			sealed, ok := auths["bob"]
			if tc.wantUID == "" {
				if ok {
					t.Error("bob's removed record is back")
				}
				return
			}
			key := session
			if tc.stored != nil {
				key = tc.stored
			}
			plain, err := decrypt(sealed, key)
			if err != nil {
				t.Fatalf("bob's record doesn't open with the key it was sealed with: %v", err)
			}
			var got CachedAuth
			if err := json.Unmarshal(plain, &got); err != nil {
				t.Fatal(err)
			}
			if got.UID != tc.wantUID {
				t.Errorf("bob's UID = %q, want %q", got.UID, tc.wantUID)
			}
		})
	}
}
