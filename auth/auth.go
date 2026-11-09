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
	"sync"
	"syscall"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/nacl/secretbox"

	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/protonmail"
)

// authFileMode lets only its owner read and write auth.json.
const authFileMode = 0o600

func authFilePath() (string, error) {
	return config.Path("auth.json")
}

// authLockFile is held while auth.json is read, changed and written back.
// It is a file of its own: saveAuths renames a new auth.json over the old
// one, and a lock on the old file would no longer guard anything.
const authLockFile = "auth.lock"

// updateAuths applies change to the records in auth.json and saves them,
// holding auth.lock throughout, so that two writers, in one process or two,
// never lose each other's change. An flock belongs to the open file, so each
// call opening the lock itself also keeps goroutines of one process apart.
func updateAuths(change func(auths map[string]string) error) error {
	p, err := config.Path(authLockFile)
	if err != nil {
		return fmt.Errorf("cannot find the auth lock: %w", err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, authFileMode)
	if err != nil {
		return fmt.Errorf("cannot open the auth lock: %w", err)
	}
	// Closing f releases the lock.
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot take the auth lock: %w", err)
	}

	auths, err := readCachedAuths()
	if err != nil {
		return err
	}
	if auths == nil {
		auths = make(map[string]string)
	}
	if err := change(auths); err != nil {
		return err
	}
	return saveAuths(auths)
}

type CachedAuth struct {
	protonmail.Auth
	LoginPassword   string
	MailboxPassword string
	KeySalts        map[string][]byte
	// TODO: add padding
}

func readCachedAuths() (map[string]string, error) {
	p, err := authFilePath()
	if err != nil {
		return nil, fmt.Errorf("failed to get auth file path: %v", err)
	}

	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to open cached auth file: %v", err)
	}
	defer f.Close()

	auths := make(map[string]string)
	if err := json.NewDecoder(f).Decode(&auths); err != nil {
		return nil, fmt.Errorf("failed to read cached auth file: %v", err)
	}

	return auths, nil
}

func saveAuths(auths map[string]string) error {
	p, err := authFilePath()
	if err != nil {
		return fmt.Errorf("failed to get auth file path: %v", err)
	}
	// Written beside it and renamed over it, so that a reader never sees it
	// half written and a crash never loses every account's credentials.
	// Usernames are in clear and the records are only as safe as the bridge
	// password: no one else may read them, whatever the umask.
	f, err := os.CreateTemp(filepath.Dir(p), ".auth.json-*")
	if err != nil {
		return fmt.Errorf("failed to create cached auth file: %v", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after the rename, there is none left to remove
	if err := f.Chmod(authFileMode); err != nil {
		f.Close()
		return fmt.Errorf("cannot restrict cached auth file to its owner: %w", err)
	}
	if err := json.NewEncoder(f).Encode(auths); err != nil {
		f.Close()
		return fmt.Errorf("failed to write cached auth file: %v", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("failed to write cached auth file: %v", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close cached auth file: %v", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("failed to replace cached auth file: %v", err)
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

func EncryptAndSave(auth *CachedAuth, username string, secretKey *[32]byte) error {
	encrypted, err := seal(auth, secretKey)
	if err != nil {
		return err
	}

	return updateAuths(func(auths map[string]string) error {
		auths[username] = encrypted
		return nil
	})
}

// errSessionNotStored is saveSession's answer when username's record is gone
// (`peroxide auth -remove`) or sealed with another bridge password (`peroxide
// auth` again) since this process logged in.
var errSessionNotStored = errors.New("the stored credentials were removed or replaced")

// saveSession stores the session of a running login, sealed with secretKey,
// only over the record it was read from: a session of an account removed or
// logged in anew since must not bring back the old record.
func saveSession(auth *CachedAuth, username string, secretKey *[32]byte) error {
	encrypted, err := seal(auth, secretKey)
	if err != nil {
		return err
	}

	return updateAuths(func(auths map[string]string) error {
		stored, ok := auths[username]
		if !ok {
			return errSessionNotStored
		}
		if _, err := decrypt(stored, secretKey); err != nil {
			return errSessionNotStored
		}
		auths[username] = encrypted
		return nil
	})
}

// saveRenewed saves the session ReAuth just renewed, logging rather than
// failing when it can't: the session works in memory either way, and failing
// the request that set off the refresh would gain nothing.
func saveRenewed(auth *CachedAuth, username string, secretKey *[32]byte) {
	err := saveSession(auth, username, secretKey)
	switch {
	case err == nil:
	case errors.Is(err, errSessionNotStored):
		log.Printf("Not saving the renewed session of %v: %v since this process logged in; it keeps the session until it exits", username, err)
	default:
		log.Printf("warning: cannot save the renewed session of %v: %v; the refresh token auth.json holds is spent, so the next start logs in with the password again", username, err)
	}
}

// isSpentRefreshToken reports whether err is Proton's answer to a refresh
// token used already: refresh tokens are single use.
func isSpentRefreshToken(err error) bool {
	apiErr, ok := err.(*protonmail.APIError)
	return ok && apiErr.Code == 10013
}

// storedAuth returns the session username has in auth.json now, which another
// peroxide process (sendmail without serve, download-mailbox) may have
// refreshed since this one read it, or nil if it can't be read.
func storedAuth(username string, secretKey *[32]byte) *protonmail.Auth {
	auths, err := readCachedAuths()
	if err != nil || auths[username] == "" {
		return nil
	}
	b, err := decrypt(auths[username], secretKey)
	if err != nil {
		return nil // sealed with another bridge password: `peroxide auth` ran
	}
	var stored CachedAuth
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil
	}
	return &stored.Auth
}

func authenticate(c *protonmail.Client, cachedAuth *CachedAuth, username string, secretKey *[32]byte) (openpgp.EntityList, error) {
	auth, err := c.AuthRefresh(&cachedAuth.Auth)
	if isSpentRefreshToken(err) {
		// Before logging in again, which Proton may answer with CAPTCHA:
		// another process may have spent the token and stored the next one.
		if stored := storedAuth(username, secretKey); stored != nil && stored.RefreshToken != cachedAuth.RefreshToken {
			log.Printf("The refresh token of %v was spent by another peroxide process: using the one it stored", username)
			auth, err = c.AuthRefresh(stored)
		}
	}
	if isSpentRefreshToken(err) {
		// Invalid refresh token, re-authenticate
		authInfo, err := c.AuthInfo(username)
		if err != nil {
			return nil, fmt.Errorf("cannot re-authenticate: failed to get auth info: %v", err)
		}

		auth, err = c.Auth(username, cachedAuth.LoginPassword, authInfo)
		if apiErr, ok := err.(*protonmail.APIError); ok && apiErr.IsHumanVerificationRequired() {
			// Human verification can't be completed from a background
			// session: it needs a browser and a human.
			return nil, fmt.Errorf("cannot re-authenticate: Proton requires human verification, please run `peroxide auth %v` again", username)
		} else if err != nil {
			return nil, fmt.Errorf("cannot re-authenticate: %v", err)
		}

		if auth.TwoFactor.Enabled != 0 {
			return nil, fmt.Errorf("cannot re-authenticate: two factor authentication enabled, please login again manually")
		}
	} else if err != nil {
		return nil, err
	}
	cachedAuth.Auth = *auth

	return c.Unlock(auth, cachedAuth.KeySalts, cachedAuth.MailboxPassword)
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
	err := updateAuths(func(auths map[string]string) error {
		if _, ok := auths[username]; !ok {
			return fmt.Errorf("no stored credentials for %v", username)
		}
		delete(auths, username)
		return nil
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

// storeKeyInfo labels the store key's derivation, so that it differs from
// the secret key sealing auth.json, which it is derived from.
const storeKeyInfo = "peroxide message store v1"

// StoreKey is the key sealing an account's stored messages.
type StoreKey [32]byte

// deriveStoreKey derives the key of the message store of the account userID
// from the secret key the bridge password encodes: HKDF-SHA256, salted with
// the account's immutable Proton user ID, so one key per account, never
// stored. The secret key is 32 random bytes: no slow KDF is needed.
func deriveStoreKey(secretKey *[32]byte, userID string) (StoreKey, error) {
	if userID == "" {
		return StoreKey{}, errors.New("cannot derive the message store key: no user ID")
	}
	b, err := hkdf.Key(sha256.New, secretKey[:], []byte(userID), storeKeyInfo, len(StoreKey{}))
	if err != nil {
		return StoreKey{}, fmt.Errorf("cannot derive the message store key: %w", err)
	}
	return StoreKey(b), nil
}

type session struct {
	hashedSecretKey []byte
	c               *protonmail.Client
	privateKeys     openpgp.EntityList
	storeKey        StoreKey
}

var ErrUnauthorized = errors.New("Invalid username or password")

type Manager struct {
	newClient func() *protonmail.Client

	// mu guards sessions and logins. A login holds its account's entry in
	// logins, a channel of one, so that two clients logging in at once don't
	// both spend the stored refresh token: Proton's are single use, and the
	// second would fail (10013) and log in again, maybe into CAPTCHA.
	mu       sync.Mutex
	sessions map[string]*session
	logins   map[string]chan struct{}
}

// loginLock returns the lock of username's login: send to take it, receive
// to give it back.
func (m *Manager) loginLock(username string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logins[username]
	if !ok {
		l = make(chan struct{}, 1)
		m.logins[username] = l
	}
	return l
}

// Auth logs username in with its bridge password, once per process, and
// returns its API client, its unlocked keys and the key sealing its stored
// messages. Logins of one account wait for each other; of others, not.
func (m *Manager) Auth(username, password string) (*protonmail.Client, openpgp.EntityList, StoreKey, error) {
	var secretKey [32]byte
	passwordBytes, err := base64.StdEncoding.DecodeString(password)
	if err != nil || len(passwordBytes) != len(secretKey) {
		return nil, nil, StoreKey{}, ErrUnauthorized
	}
	copy(secretKey[:], passwordBytes)

	login := m.loginLock(username)
	login <- struct{}{}
	defer func() { <-login }()

	m.mu.Lock()
	s, ok := m.sessions[username]
	m.mu.Unlock()
	if ok {
		err := bcrypt.CompareHashAndPassword(s.hashedSecretKey, secretKey[:])
		if err != nil {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}
	} else {
		auths, err := readCachedAuths()
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, StoreKey{}, err
		}

		encrypted, ok := auths[username]
		if !ok {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}

		decrypted, err := decrypt(encrypted, &secretKey)
		if err != nil {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}

		var cachedAuth CachedAuth
		if err := json.Unmarshal(decrypted, &cachedAuth); err != nil {
			return nil, nil, StoreKey{}, err
		}

		c := m.newClient()
		c.ReAuth = func() error {
			if _, err := authenticate(c, &cachedAuth, username, &secretKey); err != nil {
				return err
			}
			saveRenewed(&cachedAuth, username, &secretKey)
			return nil
		}

		// authenticate updates cachedAuth with the new refresh token
		privateKeys, err := authenticate(c, &cachedAuth, username, &secretKey)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		// Unlike a refresh's, a failed save fails the login: the record may
		// be gone (removed: refuse it) or auth.json unwritable.
		if err := saveSession(&cachedAuth, username, &secretKey); err != nil {
			return nil, nil, StoreKey{}, fmt.Errorf("cannot save the session of %v: %w", username, err)
		}

		// The user ID from the account itself: the cached Auth holds what
		// the last /auth/refresh returned, which need not carry it. One
		// request per account and process.
		pu, err := c.GetCurrentUser()
		if err != nil {
			return nil, nil, StoreKey{}, fmt.Errorf("cannot get the account: %w", err)
		}
		storeKey, err := deriveStoreKey(&secretKey, pu.ID)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		hashed, err := bcrypt.GenerateFromPassword(secretKey[:], bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		s = &session{
			c:               c,
			privateKeys:     privateKeys,
			hashedSecretKey: hashed,
			storeKey:        storeKey,
		}
		m.mu.Lock()
		m.sessions[username] = s
		m.mu.Unlock()
	}

	return s.c, s.privateKeys, s.storeKey, nil
}

func NewManager(newClient func() *protonmail.Client) *Manager {
	return &Manager{
		newClient: newClient,
		sessions:  make(map[string]*session),
		logins:    make(map[string]chan struct{}),
	}
}
