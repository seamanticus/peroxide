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

// refresh renews the session current with its refresh token. A token spent
// by another peroxide process (sendmail without serve, download-mailbox) is
// retried with the one that process stored in session.json.
func refresh(c *protonmail.Client, current *protonmail.Auth, username string, sessionKey *[32]byte) (*protonmail.Auth, error) {
	auth, err := c.AuthRefresh(current)
	if isSpentRefreshToken(err) {
		if stored := storedSession(username, sessionKey); stored != nil && stored.RefreshToken != current.RefreshToken {
			log.Printf("The refresh token of %v was spent by another peroxide process: using the one it stored", username)
			auth, err = c.AuthRefresh(stored)
		}
	}
	return auth, err
}

// needsAuthError is a login again with the stored password that only
// `peroxide auth` can mend: Proton asks for human verification or a second
// factor, which a client's login can't answer.
type needsAuthError struct{ msg string }

func (e *needsAuthError) Error() string { return e.msg }

// loginAgain logs username in to Proton with the login password of its
// record, for a session whose refresh token Proton refuses.
func loginAgain(c *protonmail.Client, record *CachedAuth, username string) (*protonmail.Auth, error) {
	authInfo, err := c.AuthInfo(username)
	if err != nil {
		return nil, fmt.Errorf("cannot re-authenticate: failed to get auth info: %v", err)
	}

	auth, err := c.Auth(username, record.LoginPassword, authInfo)
	if apiErr, ok := err.(*protonmail.APIError); ok && apiErr.IsHumanVerificationRequired() {
		// Human verification can't be completed from a client's login: it
		// needs a browser and a human.
		return nil, &needsAuthError{fmt.Sprintf("cannot re-authenticate: Proton requires human verification, please run `peroxide auth %v` again", username)}
	} else if err != nil {
		return nil, fmt.Errorf("cannot re-authenticate: %v", err)
	}

	if auth.TwoFactor.Enabled != 0 {
		return nil, &needsAuthError{fmt.Sprintf("cannot re-authenticate: two factor authentication enabled, please run `peroxide auth %v` again", username)}
	}
	return auth, nil
}

// opened is what openSession gives a session to keep.
type opened struct {
	auth        protonmail.Auth
	sessionKey  [32]byte
	privateKeys openpgp.EntityList
}

// openSession gets username a working Proton session and its unlocked keys
// from its auth.json record, with the secret key of a client's login: the
// only time the passwords are read. It renews the session last saved
// (session.json, else the record's own); if its refresh token is spent, it
// logs in again with the login password. The new tokens are saved, and a
// failed save fails the login: the record may be gone (removed: refuse it)
// or session.json unwritable.
func openSession(c *protonmail.Client, record *CachedAuth, id recordID, username string, secretKey *[32]byte) (*opened, error) {
	sessionKey, err := deriveSessionKey(secretKey, username)
	if err != nil {
		return nil, err
	}
	current := &record.Auth
	if stored := storedSession(username, &sessionKey); stored != nil {
		current = stored
	}

	auth, err := refresh(c, current, username, &sessionKey)
	if isSpentRefreshToken(err) {
		log.Printf("The refresh token of %v is spent: logging in again", username)
		auth, err = loginAgain(c, record, username)
	}
	if err != nil {
		return nil, err
	}

	privateKeys, err := c.Unlock(auth, record.KeySalts, record.MailboxPassword)
	if err != nil {
		return nil, err
	}
	if err := saveSession(auth, username, &sessionKey, id); err != nil {
		return nil, fmt.Errorf("cannot save the session of %v: %w", username, err)
	}
	return &opened{auth: *auth, sessionKey: sessionKey, privateKeys: privateKeys}, nil
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
	storeKey        StoreKey
	// Gets a value when the session dies (renew), taken back when it is
	// opened again: buffered, so that renew never waits for a reader.
	died chan struct{}

	// Below, under Manager.mu.

	privateKeys openpgp.EntityList
	// What renews the session and saves it, and nothing more: the tokens,
	// the key of its session.json record and the ID of the auth.json record
	// it was read from. The passwords and the secret key are not kept.
	auth       protonmail.Auth
	sessionKey [32]byte
	record     recordID
	// Set when the refresh token is spent: requests fail until a client's
	// login, which brings the bridge password, opens the session again.
	dead bool
	// Set by MarkStale: the account has a key that isn't in privateKeys.
	stale bool
	// The IDs of the keys MarkStale was given: a key that stays locked is
	// reported once.
	tried map[string]struct{}
}

// refusal is a login again that needs `peroxide auth` (needsAuthError), and
// the auth.json record it was tried with.
type refusal struct {
	record recordID
	err    error
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
	// The accounts whose stored password Proton won't take without a human:
	// their logins are refused at once, with no request, until `peroxide
	// auth` stores a new record. A client retrying its login would otherwise
	// send Proton the password again each time.
	refused map[string]refusal
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

// readRecord opens username's record in auth.json with secretKey, and
// returns the ID of the sealed record with it.
func readRecord(username string, secretKey *[32]byte) (*CachedAuth, recordID, error) {
	auths, err := readCachedAuths()
	if err != nil && !os.IsNotExist(err) {
		return nil, recordID{}, err
	}

	encrypted, ok := auths[username]
	if !ok {
		return nil, recordID{}, ErrUnauthorized
	}

	decrypted, err := decrypt(encrypted, secretKey)
	if err != nil {
		return nil, recordID{}, ErrUnauthorized
	}

	var cachedAuth CachedAuth
	if err := json.Unmarshal(decrypted, &cachedAuth); err != nil {
		return nil, recordID{}, err
	}
	return &cachedAuth, recordIDOf(encrypted), nil
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
	record, _, err := readRecord(username, secretKey)
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

// errSessionDead fails the requests of a session Proton won't renew.
var errSessionDead = errors.New("the Proton session has ended: log in again from your mail client")

// SessionDied returns the channel that gets a value when username's Proton
// session dies: its refresh token is spent, and only a client's login, which
// brings the bridge password, can open it again (Auth). A caller serving
// clients has them log in again. It is nil without a session.
func (m *Manager) SessionDied(username string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[username]; ok {
		return s.died
	}
	return nil
}

// renew is the client's ReAuth: it renews s with its refresh token and saves
// the new one. It has no password to log in again with: if the token is
// spent, the session is dead until a client's login revives it (Auth), which
// SessionDied's reader hastens.
func (m *Manager) renew(s *session, username string) error {
	m.mu.Lock()
	current, sessionKey, record, dead := s.auth, s.sessionKey, s.record, s.dead
	m.mu.Unlock()
	if dead {
		return errSessionDead
	}

	auth, err := refresh(s.c, &current, username, &sessionKey)
	if isSpentRefreshToken(err) {
		log.Printf("The refresh token of %v is spent: its session is opened again at the next login", username)
		m.mu.Lock()
		s.dead = true
		m.mu.Unlock()
		select {
		case s.died <- struct{}{}:
		default:
		}
		return fmt.Errorf("%w (%v)", errSessionDead, err)
	}
	if err != nil {
		return err
	}
	s.c.UseSession(auth)
	m.mu.Lock()
	s.auth = *auth
	m.mu.Unlock()
	saveRenewed(auth, username, &sessionKey, record)
	return nil
}

// open reads username's record with secretKey and opens a session from it
// (openSession), unless that record was refused before.
func (m *Manager) open(c *protonmail.Client, username string, secretKey *[32]byte) (*opened, recordID, error) {
	record, id, err := readRecord(username, secretKey)
	if err != nil {
		return nil, recordID{}, err
	}
	m.mu.Lock()
	r, ok := m.refused[username]
	m.mu.Unlock()
	if ok && r.record == id {
		return nil, recordID{}, r.err
	}
	o, err := openSession(c, record, id, username, secretKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := errors.AsType[*needsAuthError](err); ok {
		m.refused[username] = refusal{record: id, err: err}
	} else {
		delete(m.refused, username)
	}
	if err != nil {
		return nil, recordID{}, err
	}
	return o, id, nil
}

// revive opens s, a dead session, again from username's record. Called
// holding username's login lock.
func (m *Manager) revive(s *session, username string, secretKey *[32]byte) error {
	o, id, err := m.open(s.c, username, secretKey)
	if err != nil {
		return err
	}
	log.Printf("Opened the Proton session of %v again", username)
	// This login is the one the signal asked for.
	select {
	case <-s.died:
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.auth, s.sessionKey, s.record = o.auth, o.sessionKey, id
	s.privateKeys = o.privateKeys
	s.dead, s.stale = false, false
	return nil
}

// Auth logs username in with its bridge password, once per process, and
// returns its API client, its unlocked keys and the key sealing its stored
// messages. Logins of one account wait for each other; of others, not. A
// session marked stale (MarkStale) unlocks its keys again first, and a dead
// one (renew) is opened again; one that died is also replaced when the
// password is that of credentials stored since (`peroxide auth` again).
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
	dead, stale := ok && s.dead, ok && s.stale
	m.mu.Unlock()
	if ok && bcrypt.CompareHashAndPassword(s.hashedSecretKey, secretKey[:]) != nil {
		if !dead {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}
		// Not this session's password: it may be that of new credentials,
		// which the record tells.
		ok = false
	}
	if ok {
		if dead {
			if err := m.revive(s, username, &secretKey); err != nil {
				return nil, nil, StoreKey{}, err
			}
		} else if stale {
			m.unlockAgain(s, username, &secretKey)
		}
	} else {
		c := m.newClient()
		o, id, err := m.open(c, username, &secretKey)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}
		s = &session{
			c:           c,
			died:        make(chan struct{}, 1),
			privateKeys: o.privateKeys,
			auth:        o.auth,
			sessionKey:  o.sessionKey,
			record:      id,
		}
		// The closure holds the session, not the record: no password and
		// no secret key outlive this call.
		c.ReAuth = func() error { return m.renew(s, username) }

		// The user ID from the account itself: the cached Auth holds what
		// the last /auth/refresh returned, which need not carry it. One
		// request per account and process.
		pu, err := c.GetCurrentUser()
		if err != nil {
			return nil, nil, StoreKey{}, fmt.Errorf("cannot get the account: %w", err)
		}
		s.storeKey, err = deriveStoreKey(&secretKey, pu.ID)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		s.hashedSecretKey, err = bcrypt.GenerateFromPassword(secretKey[:], bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, StoreKey{}, err
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
		refused:   make(map[string]refusal),
	}
}
