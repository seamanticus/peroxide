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
	// Set by MarkStale, under Manager.mu: the account has a key that isn't
	// in privateKeys.
	stale bool
	// The IDs of the keys MarkStale was given, under Manager.mu: a key that
	// stays locked is reported once.
	tried map[string]struct{}
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

// readRecord opens username's record in auth.json with secretKey.
func readRecord(username string, secretKey *[32]byte) (*CachedAuth, error) {
	auths, err := readCachedAuths()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	encrypted, ok := auths[username]
	if !ok {
		return nil, ErrUnauthorized
	}

	decrypted, err := decrypt(encrypted, secretKey)
	if err != nil {
		return nil, ErrUnauthorized
	}

	var cachedAuth CachedAuth
	if err := json.Unmarshal(decrypted, &cachedAuth); err != nil {
		return nil, err
	}
	return &cachedAuth, nil
}

// MarkStale records that username's account has the key keyID, which its
// session hasn't unlocked (a new address, a key rotation). Nothing that
// unlocks keys is kept for it: the next Auth of the account, which brings
// the bridge password, unlocks them again. It reports whether the key is
// news: false for a key marked before, which that Auth couldn't unlock or
// hasn't yet, and without a session, whose first Auth unlocks anyway.
func (m *Manager) MarkStale(username, keyID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[username]
	if !ok {
		return false
	}
	if _, ok := s.tried[keyID]; ok {
		return false
	}
	if s.tried == nil {
		s.tried = make(map[string]struct{})
	}
	s.tried[keyID] = struct{}{}
	s.stale = true
	return true
}

// unlockAgain replaces the keys of s, a stale session, with those the
// mailbox password in auth.json unlocks now. A failure leaves the keys and
// the mark as they were: the login goes on with the keys it had, and the
// next one tries again. Called holding username's login lock.
func (m *Manager) unlockAgain(s *session, username string, secretKey *[32]byte) {
	record, err := readRecord(username, secretKey)
	if err != nil {
		log.Printf("cannot unlock the new keys of %v: cannot read its credentials: %v", username, err)
		return
	}
	privateKeys, err := s.c.UnlockAgain(record.KeySalts, record.MailboxPassword)
	if err != nil {
		log.Printf("cannot unlock the new keys of %v: %v", username, err)
		return
	}
	log.Printf("Unlocked the keys of %v again: %v keys", username, len(privateKeys))
	m.mu.Lock()
	defer m.mu.Unlock()
	s.privateKeys = privateKeys
	s.stale = false
}

// Auth logs username in with its bridge password, once per process, and
// returns its API client, its unlocked keys and the key sealing its stored
// messages. Logins of one account wait for each other; of others, not. A
// session marked stale (MarkStale) unlocks its keys again first.
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
		m.mu.Lock()
		stale := s.stale
		m.mu.Unlock()
		if stale {
			m.unlockAgain(s, username, &secretKey)
		}
	} else {
		cachedAuth, err := readRecord(username, &secretKey)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		c := m.newClient()
		c.ReAuth = func() error {
			if _, err := authenticate(c, cachedAuth, username, &secretKey); err != nil {
				return err
			}
			saveRenewed(cachedAuth, username, &secretKey)
			return nil
		}

		// authenticate updates cachedAuth with the new refresh token
		privateKeys, err := authenticate(c, cachedAuth, username, &secretKey)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		// Unlike a refresh's, a failed save fails the login: the record may
		// be gone (removed: refuse it) or auth.json unwritable.
		if err := saveSession(cachedAuth, username, &secretKey); err != nil {
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

	m.mu.Lock()
	defer m.mu.Unlock()
	return s.c, s.privateKeys, s.storeKey, nil
}

func NewManager(newClient func() *protonmail.Client) *Manager {
	return &Manager{
		newClient: newClient,
		sessions:  make(map[string]*session),
		logins:    make(map[string]chan struct{}),
	}
}
