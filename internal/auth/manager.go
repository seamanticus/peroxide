package auth

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/bcrypt"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// isSpentRefreshToken reports whether err is Proton's answer to a refresh
// token used already: refresh tokens are single use.
func isSpentRefreshToken(err error) bool {
	apiErr, ok := err.(*protonmail.APIError)
	return ok && apiErr.Code == 10013
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
