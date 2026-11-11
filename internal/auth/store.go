package auth

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/crypto/nacl/secretbox"

	"github.com/seamanticus/peroxide/internal/paths"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// authFileMode lets only its owner read and write auth.json.
const authFileMode = 0o600

// authFile holds each account's credentials, sealed with its bridge
// password: written by `peroxide auth` only. sessionFile holds each
// account's Proton session (its tokens), which every refresh replaces,
// sealed with a key derived from the bridge password (deriveSessionKey): a
// running session keeps that key, which opens nothing else, rather than the
// bridge password.
const (
	authFile    = "auth.json"
	sessionFile = "session.json"
)

// authLockFile is held while auth.json is read, changed and written back.
// It is a file of its own: saveAuths renames a new auth.json over the old
// one, and a lock on the old file would no longer guard anything. It guards
// session.json too.
const authLockFile = "auth.lock"

// withAuthLock runs f holding auth.lock, so that two writers of auth.json or
// session.json, in one process or two, never lose each other's change. An
// flock belongs to the open file, so each call opening the lock itself also
// keeps goroutines of one process apart.
func withAuthLock(f func() error) error {
	p, err := paths.Path(authLockFile)
	if err != nil {
		return fmt.Errorf("cannot find the auth lock: %w", err)
	}
	lock, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, authFileMode)
	if err != nil {
		return fmt.Errorf("cannot open the auth lock: %w", err)
	}
	// Closing lock releases it.
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot take the auth lock: %w", err)
	}
	return f()
}

type CachedAuth struct {
	protonmail.Auth
	LoginPassword   string
	MailboxPassword string
	KeySalts        map[string][]byte
	// TODO: add padding
}

func readCachedAuths() (map[string]string, error) {
	return readSealed(authFile)
}

func saveAuths(auths map[string]string) error {
	return writeSealed(authFile, auths)
}

func authFilePath() (string, error) {
	return paths.Path(authFile)
}

// readSealed reads the file name: username -> sealed record. No file is no
// records.
func readSealed(name string) (map[string]string, error) {
	p, err := paths.Path(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get the path of %v: %v", name, err)
	}

	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to open %v: %v", name, err)
	}
	defer f.Close()

	auths := make(map[string]string)
	if err := json.NewDecoder(f).Decode(&auths); err != nil {
		return nil, fmt.Errorf("failed to read %v: %v", name, err)
	}

	return auths, nil
}

// writeSealed replaces the file name with records.
func writeSealed(name string, records map[string]string) error {
	p, err := paths.Path(name)
	if err != nil {
		return fmt.Errorf("failed to get the path of %v: %v", name, err)
	}
	// Written beside it and renamed over it, so that a reader never sees it
	// half written and a crash never loses every account's credentials.
	// Usernames are in clear and the records are only as safe as the bridge
	// password: no one else may read them, whatever the umask.
	f, err := os.CreateTemp(filepath.Dir(p), "."+name+"-*")
	if err != nil {
		return fmt.Errorf("failed to create %v: %v", name, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename, there is none left to remove
	if err := f.Chmod(authFileMode); err != nil {
		f.Close()
		return fmt.Errorf("cannot restrict %v to its owner: %w", name, err)
	}
	if err := json.NewEncoder(f).Encode(records); err != nil {
		f.Close()
		return fmt.Errorf("failed to write %v: %v", name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("failed to write %v: %v", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close %v: %v", name, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("failed to replace %v: %v", name, err)
	}
	return nil
}

func encrypt(msg []byte, secretKey *[32]byte) (string, error) {
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %v", err)
	}

	encrypted := secretbox.Seal(nonce[:], msg, &nonce, secretKey)
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func decrypt(encryptedString string, secretKey *[32]byte) ([]byte, error) {
	encrypted, err := base64.StdEncoding.DecodeString(encryptedString)
	if err != nil {
		return nil, fmt.Errorf("invalid encrypted string: %v", err)
	}

	var nonce [24]byte
	copy(nonce[:], encrypted[:24])
	decrypted, ok := secretbox.Open(nil, encrypted[24:], &nonce, secretKey)
	if !ok {
		return nil, errors.New("decryption error")
	}
	return decrypted, nil
}

func seal(auth *CachedAuth, secretKey *[32]byte) (string, error) {
	cleartext, err := json.Marshal(auth)
	if err != nil {
		return "", fmt.Errorf("failed to format cached auth: %v", err)
	}
	return encrypt(cleartext, secretKey)
}

// dropSession removes username's record from session.json, if it has one.
// Called holding auth.lock.
func dropSession(username string) error {
	sessions, err := readSealed(sessionFile)
	if err != nil {
		return err
	}
	if _, ok := sessions[username]; !ok {
		return nil
	}
	delete(sessions, username)
	return writeSealed(sessionFile, sessions)
}

// EncryptAndSave stores username's credentials in auth.json, sealed with
// secretKey, as `peroxide auth` does: nothing else writes that file. The
// session an earlier login left in session.json goes: auth.Auth is the new
// one.
func EncryptAndSave(auth *CachedAuth, username string, secretKey *[32]byte) error {
	encrypted, err := seal(auth, secretKey)
	if err != nil {
		return err
	}

	return withAuthLock(func() error {
		auths, err := readCachedAuths()
		if err != nil {
			return err
		}
		if auths == nil {
			auths = make(map[string]string)
		}
		auths[username] = encrypted
		if err := saveAuths(auths); err != nil {
			return err
		}
		return dropSession(username)
	})
}

// sessionKeyInfo labels the session key's derivation, so that it differs
// from the secret key and from the store key.
const sessionKeyInfo = "peroxide session v1"

// deriveSessionKey derives the key sealing username's record in
// session.json from the secret key its bridge password encodes
// (HKDF-SHA256, salted with the username). A running session keeps it to
// save its renewed tokens: it opens only that record, which holds what the
// session has in memory anyway, and the secret key can't be had from it.
func deriveSessionKey(secretKey *[32]byte, username string) ([32]byte, error) {
	b, err := hkdf.Key(sha256.New, secretKey[:], []byte(username), sessionKeyInfo, 32)
	if err != nil {
		return [32]byte{}, fmt.Errorf("cannot derive the session key: %w", err)
	}
	return [32]byte(b), nil
}

// recordID names a sealed auth.json record without revealing it: a session
// keeps the ID of the record it was read from.
type recordID [sha256.Size]byte

func recordIDOf(sealed string) recordID { return sha256.Sum256([]byte(sealed)) }

// errSessionNotStored is saveSession's answer when username's auth.json
// record is gone (`peroxide auth -remove`) or replaced (`peroxide auth`
// again) since this process logged in.
var errSessionNotStored = errors.New("the stored credentials were removed or replaced")

// saveSession stores the tokens of a running login in session.json, sealed
// with sessionKey, only while auth.json still holds the record the login was
// read from: a session of an account removed or logged in anew since must
// leave nothing behind.
func saveSession(auth *protonmail.Auth, username string, sessionKey *[32]byte, record recordID) error {
	cleartext, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("failed to format the session: %v", err)
	}
	encrypted, err := encrypt(cleartext, sessionKey)
	if err != nil {
		return err
	}

	return withAuthLock(func() error {
		auths, err := readCachedAuths()
		if err != nil {
			return err
		}
		stored, ok := auths[username]
		if !ok || recordIDOf(stored) != record {
			return errSessionNotStored
		}
		sessions, err := readSealed(sessionFile)
		if err != nil {
			return err
		}
		if sessions == nil {
			sessions = make(map[string]string)
		}
		sessions[username] = encrypted
		return writeSealed(sessionFile, sessions)
	})
}

// saveRenewed saves the session a refresh just renewed, logging rather than
// failing when it can't: the session works in memory either way, and failing
// the request that set off the refresh would gain nothing.
func saveRenewed(auth *protonmail.Auth, username string, sessionKey *[32]byte, record recordID) {
	err := saveSession(auth, username, sessionKey, record)
	switch {
	case err == nil:
	case errors.Is(err, errSessionNotStored):
		log.Printf("Not saving the renewed session of %v: %v since this process logged in; it keeps the session until it exits", username, err)
	default:
		log.Printf("warning: cannot save the renewed session of %v: %v; the refresh token session.json holds is spent, so the next start logs in with the password again", username, err)
	}
}

// storedSession returns the session username has in session.json now, which
// another peroxide process (sendmail without serve, download-mailbox) may
// have refreshed since this one read it, or nil if there is none this key
// opens: none saved yet, or one of an earlier bridge password.
func storedSession(username string, sessionKey *[32]byte) *protonmail.Auth {
	sessions, err := readSealed(sessionFile)
	if err != nil || sessions[username] == "" {
		return nil
	}
	b, err := decrypt(sessions[username], sessionKey)
	if err != nil {
		return nil
	}
	var stored protonmail.Auth
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil
	}
	return &stored
}

func ListUsernames() ([]string, error) {
	auths, err := readCachedAuths()
	if err != nil {
		return nil, err
	}

	l := make([]string, 0, len(auths))
	for username, _ := range auths {
		l = append(l, username)
	}
	return l, nil
}

func DeleteUsername(username string) error {
	err := withAuthLock(func() error {
		auths, err := readCachedAuths()
		if err != nil {
			return err
		}
		if _, ok := auths[username]; !ok {
			return fmt.Errorf("no stored credentials for %v", username)
		}
		delete(auths, username)
		if err := saveAuths(auths); err != nil {
			return err
		}
		return dropSession(username)
	})
	if err != nil {
		return err
	}
	log.Printf("Deleted credentials for %v", username)
	return nil
}

func GeneratePassword() (secretKey *[32]byte, password string, err error) {
	var key [32]byte
	if _, err = io.ReadFull(rand.Reader, key[:]); err != nil {
		return nil, "", fmt.Errorf("failed to generate key: %v", err)
	}
	password = base64.StdEncoding.EncodeToString(key[:])
	return &key, password, nil
}
