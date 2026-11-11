package auth

import (
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"

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
		if stored := loggedStoredSession(username, sessionKey); stored != nil && stored.RefreshToken != current.RefreshToken {
			log.Printf("The refresh token of %v was spent by another peroxide process: using the one it stored", username)
			auth, err = c.AuthRefresh(stored)
		}
	}
	return auth, err
}

// loggedStoredSession is storedSession, logging rather than returning its
// error: a session.json that can't be read only costs a login with the
// password, which is better than no login at all, but must not go unseen.
func loggedStoredSession(username string, sessionKey *[32]byte) *protonmail.Auth {
	stored, err := storedSession(username, sessionKey)
	if err != nil {
		log.Printf("warning: cannot read the saved session of %v, using the one of its stored credentials: %v", username, err)
		return nil
	}
	return stored
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
// logs in again with the login password. The new tokens are saved. A record
// gone or replaced meanwhile fails the login; a session.json that can't be
// written only warns, as the refresh has spent the token saved before: the
// session just got is the only one left.
func openSession(c *protonmail.Client, record *CachedAuth, id recordID, username string, secretKey *[32]byte) (*opened, error) {
	sessionKey, err := deriveSessionKey(secretKey, username)
	if err != nil {
		return nil, err
	}
	current := &record.Auth
	if stored := loggedStoredSession(username, &sessionKey); stored != nil {
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
	if err := saveSession(auth, username, &sessionKey, id); errors.Is(err, errSessionNotStored) {
		return nil, fmt.Errorf("cannot save the session of %v: %w", username, err)
	} else if err != nil {
		log.Printf("warning: cannot save the session of %v: %v; the next start logs in with the password again", username, err)
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
	// What a login's bridge password is checked against (deriveVerifier);
	// neither the password nor the secret key is kept.
	verifier [32]byte
	c        *protonmail.Client
	storeKey StoreKey
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
	// Set by endSession, with dead: the session's record is gone or
	// replaced, and it is never opened again.
	ended bool
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
	// logins (lockLogin), so that two clients logging in at once don't
	// both spend the stored refresh token: Proton's are single use, and the
	// second would fail (10013) and log in again, maybe into CAPTCHA.
	mu       sync.Mutex
	sessions map[string]*session
	logins   map[string]*loginLock
	// The accounts whose stored password Proton won't take without a human:
	// their logins are refused at once, with no request, until `peroxide
	// auth` stores a new record. A client retrying its login would otherwise
	// send Proton the password again each time.
	refused map[string]refusal
}

// loginLock is the lock of one account's logins: a channel of one, and the
// number of logins holding it or waiting for it. The entry goes when that is
// zero, so that logins for usernames that don't exist leave nothing behind.
type loginLock struct {
	ch    chan struct{}
	users int
}

// lockLogin takes the lock of username's logins, and returns what gives it
// back.
func (m *Manager) lockLogin(username string) (unlock func()) {
	m.mu.Lock()
	l, ok := m.logins[username]
	if !ok {
		l = &loginLock{ch: make(chan struct{}, 1)}
		m.logins[username] = l
	}
	l.users++
	m.mu.Unlock()
	l.ch <- struct{}{}
	return func() {
		<-l.ch
		m.mu.Lock()
		defer m.mu.Unlock()
		l.users--
		if l.users == 0 {
			delete(m.logins, username)
		}
	}
}

// readRecord opens username's record in auth.json with secretKey, and
// returns the ID of the sealed record with it. A file it can't read is
// logged and answered ErrUnauthorized: the login isn't authenticated yet,
// and its client must learn nothing about peroxide's files.
func readRecord(username string, secretKey *[32]byte) (*CachedAuth, recordID, error) {
	auths, err := readCachedAuths()
	if err != nil {
		log.Printf("Refusing a login of %v: cannot read the stored credentials: %v", username, err)
		return nil, recordID{}, ErrUnauthorized
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
// SessionDied's reader hastens. If its auth.json record is gone or replaced,
// the renewed session is not saved and is ended (endSession), so clients
// still connected are cut off by the next refresh at the latest.
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
	if !saveRenewed(auth, username, &sessionKey, record) {
		m.endSession(s, username, auth)
		return errSessionEnded
	}
	m.mu.Lock()
	if s.ended {
		// Ended by a login while this refresh ran: the tokens it just got
		// must not arm the client again.
		m.mu.Unlock()
		m.logout(s, username, auth)
		return errSessionEnded
	}
	s.auth = *auth
	s.c.UseSession(auth)
	m.mu.Unlock()
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
// holding username's login lock. A session ended meanwhile (by a renewal
// that found its record changed) is not.
func (m *Manager) revive(s *session, username string, secretKey *[32]byte) error {
	m.mu.Lock()
	ended := s.ended
	m.mu.Unlock()
	if ended {
		return ErrUnauthorized
	}
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

// errSessionEnded fails the requests of a session ended by endSession.
var errSessionEnded = errors.New("the stored credentials were removed or replaced: log in again from your mail client with the new bridge password")

// endSession ends s, username's session, for good: the credentials it was
// read from are gone or replaced (`peroxide auth -remove`, or `peroxide
// auth` again). It is logged out at Proton, with fresh, the tokens a refresh
// just got, if not nil: whoever copied its tokens can't use them either.
// Its client then sends no token, its renewals fail, and its clients are
// told to log in again (SessionDied): the old bridge password no longer logs
// in, and the new one opens a new session. A login ending it holds
// username's login lock; a renewal ending it doesn't need to, as a session
// no longer in m.sessions is never revived.
func (m *Manager) endSession(s *session, username string, fresh *protonmail.Auth) {
	m.mu.Lock()
	if s.ended {
		m.mu.Unlock()
		// A renewal and a login both found the record changed. The first
		// logged out the tokens it had; a refresh's may be newer.
		if fresh != nil {
			m.logout(s, username, fresh)
		}
		return
	}
	log.Printf("The stored credentials of %v were removed or replaced: ending the session read from them", username)
	s.dead, s.ended = true, true
	s.privateKeys = nil
	tokens := s.auth
	if fresh != nil {
		tokens = *fresh
	}
	// Under mu, as renew arms the client: no request is sent with the
	// session's tokens from now on.
	s.c.UseSession(&protonmail.Auth{})
	if m.sessions[username] == s {
		delete(m.sessions, username)
	}
	m.mu.Unlock()
	m.logout(s, username, &tokens)
	select {
	case s.died <- struct{}{}:
	default:
	}
}

// logout ends at Proton the session of tokens, which s held or a refresh of
// it just got. A refusal is logged: a dead session's access token may have
// expired already, and only Proton's session management can end it then.
func (m *Manager) logout(s *session, username string, tokens *protonmail.Auth) {
	if err := s.c.Logout(tokens); err != nil {
		log.Printf("Cannot log out the ended session of %v at Proton: %v; revoke it in Proton's session management if its tokens may have leaked", username, err)
		return
	}
	log.Printf("Logged out the ended session of %v at Proton", username)
}

// recordChanged reports whether username's auth.json record is no longer the
// one s was read from. A file it can't read is logged and reported as an
// error with no detail, for a client not yet authenticated.
func recordChanged(s *session, username string) (bool, error) {
	id, ok, err := storedRecordID(username)
	if err != nil {
		log.Printf("Refusing a login of %v: cannot read the stored credentials: %v", username, err)
		return false, ErrUnauthorized
	}
	return !ok || id != s.record, nil
}

// verifierInfo labels the derivation of a session's verifier, so that it
// differs from the secret key, the store key and the session key.
const verifierInfo = "peroxide login verifier v1"

// deriveVerifier derives what a session checks a login's bridge password
// against: HKDF-SHA256 of the secret key it encodes, salted with the
// username. The secret key is 32 random bytes, so no slow hash is needed:
// guessing it is out of reach at any speed, and auth.json already tests a
// guess with one fast decryption. It is one-way: the secret key, which opens
// auth.json, can't be had from it.
func deriveVerifier(secretKey *[32]byte, username string) ([32]byte, error) {
	b, err := hkdf.Key(sha256.New, secretKey[:], []byte(username), verifierInfo, 32)
	if err != nil {
		return [32]byte{}, fmt.Errorf("cannot derive the login verifier: %w", err)
	}
	return [32]byte(b), nil
}

// matches reports whether secretKey is that of the bridge password s was
// opened with. Constant time, as is customary for secrets.
func (s *session) matches(secretKey *[32]byte, username string) (bool, error) {
	v, err := deriveVerifier(secretKey, username)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(v[:], s.verifier[:]) == 1, nil
}

// decodeBridgePassword returns the secret key a bridge password encodes. Only
// the form `peroxide auth` prints is taken: 44 characters of standard,
// padded base64 whose spare bits are zero, with no line breaks, so that one
// key has one password.
func decodeBridgePassword(password string) (*[32]byte, bool) {
	var secretKey [32]byte
	if len(password) != base64.StdEncoding.EncodedLen(len(secretKey)) {
		return nil, false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(password)
	if err != nil || len(b) != len(secretKey) {
		return nil, false
	}
	copy(secretKey[:], b)
	return &secretKey, true
}

// Auth logs username in with its bridge password, once per process, and
// returns its API client, its unlocked keys and the key sealing its stored
// messages. Logins of one account wait for each other; of others, not. A
// session marked stale (MarkStale) unlocks its keys again first, and a dead
// one (renew) is opened again. A session whose auth.json record is gone or
// replaced is ended (endSession) by a login with its own bridge password,
// which is then refused, or with the password of the record stored now,
// which opens a new session: a login with neither is refused and ends
// nothing, so that no client can disconnect another without a password.
func (m *Manager) Auth(username, password string) (*protonmail.Client, openpgp.EntityList, StoreKey, error) {
	secretKey, ok := decodeBridgePassword(password)
	if !ok {
		return nil, nil, StoreKey{}, ErrUnauthorized
	}

	defer m.lockLogin(username)()

	m.mu.Lock()
	old, ok := m.sessions[username]
	m.mu.Unlock()
	if ok {
		changed, err := recordChanged(old, username)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}
		match, err := old.matches(secretKey, username)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}
		switch {
		case !changed && !match:
			return nil, nil, StoreKey{}, ErrUnauthorized
		case !changed:
			return m.reuse(old, username, secretKey)
		case match:
			// The old password: proven, so the session ends, and refused.
			m.endSession(old, username, nil)
			return nil, nil, StoreKey{}, ErrUnauthorized
		}
		// Maybe the new record's password: open below, then end old.
	}

	c := m.newClient()
	o, id, err := m.open(c, username, secretKey)
	if err != nil {
		return nil, nil, StoreKey{}, err
	}
	if ok {
		m.endSession(old, username, nil)
	}
	s := &session{
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
	s.storeKey, err = deriveStoreKey(secretKey, pu.ID)
	if err != nil {
		return nil, nil, StoreKey{}, err
	}

	s.verifier, err = deriveVerifier(secretKey, username)
	if err != nil {
		return nil, nil, StoreKey{}, err
	}

	m.mu.Lock()
	m.sessions[username] = s
	m.mu.Unlock()
	return m.result(s)
}

// reuse logs in to s, username's session, which secretKey matched: it is
// opened again if dead, its keys unlocked again if stale. Called holding
// username's login lock.
func (m *Manager) reuse(s *session, username string, secretKey *[32]byte) (*protonmail.Client, openpgp.EntityList, StoreKey, error) {
	m.mu.Lock()
	dead, stale := s.dead, s.stale
	m.mu.Unlock()
	if dead {
		if err := m.revive(s, username, secretKey); err != nil {
			return nil, nil, StoreKey{}, err
		}
	} else if stale {
		m.unlockAgain(s, username, secretKey)
	}
	return m.result(s)
}

// result is what Auth returns for s.
func (m *Manager) result(s *session) (*protonmail.Client, openpgp.EntityList, StoreKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.dead {
		// Ended or died by a renewal while this login ran.
		return nil, nil, StoreKey{}, errSessionDead
	}
	return s.c, s.privateKeys, s.storeKey, nil
}

func NewManager(newClient func() *protonmail.Client) *Manager {
	return &Manager{
		newClient: newClient,
		sessions:  make(map[string]*session),
		logins:    make(map[string]*loginLock),
		refused:   make(map[string]refusal),
	}
}
