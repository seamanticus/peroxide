package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/go-crypto/openpgp/s2k"

	"github.com/seamanticus/peroxide/internal/paths"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// lockedKey returns a new private key as Proton keeps it: armored, locked
// with passphrase. The passphrase is stretched as little as OpenPGP allows
// (65536 bytes hashed, against 16 MiB by default): every login of a test
// unlocks its keys again, and the race detector slows hashing several times.
func lockedKey(t *testing.T, id, passphrase string) *protonmail.PrivateKey {
	t.Helper()
	e, err := openpgp.NewEntity(id, "", id+"@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	cheap := &packet.Config{S2KConfig: &s2k.Config{S2KMode: s2k.IteratedSaltedS2K, S2KCount: 65536}}
	if err := e.EncryptPrivateKeys([]byte(passphrase), cheap); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	aw, err := armor.Encode(&b, "PGP PRIVATE KEY BLOCK", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivateWithoutSigning(aw, nil); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return &protonmail.PrivateKey{ID: id, Active: 1, PrivateKey: b.String()}
}

// A session keeps the keys of its first login: later logins make no request.
// Once marked stale for a key (an address event brought one that isn't
// unlocked), the next login, and only that one, reads the credentials again
// and unlocks the keys the account has now. A key is news once.
// evidence: witnesses
func TestAuthUnlocksAgainWhenStale(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	secretKey, password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "r", MailboxPassword: "mbox"}, "bob", secretKey); err != nil {
		t.Fatal(err)
	}

	user := &protonmail.User{ID: "uid", Name: "bob", Keys: []*protonmail.PrivateKey{lockedKey(t, "user", "mbox")}}
	addr := &protonmail.Address{ID: "a", Email: "bob@example.org", Keys: []*protonmail.PrivateKey{lockedKey(t, "k1", "mbox")}}
	var requests []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		var body any
		switch r.URL.Path {
		case "/auth/refresh":
			body = map[string]any{"Code": 1000, "UID": "u", "AccessToken": "a", "RefreshToken": "next", "ExpiresIn": 3600}
		case "/users":
			body = map[string]any{"Code": 1000, "User": user}
		case "/addresses":
			body = map[string]any{"Code": 1000, "Addresses": []*protonmail.Address{addr}}
		default:
			t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Error(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(bytes.NewReader(b)), Request: r}, nil
	})
	m := NewManager(func() *protonmail.Client {
		return &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
	})
	login := func(step string, wantKeys int, wantRequests ...string) {
		t.Helper()
		requests = nil
		_, keys, _, err := m.Auth("bob", password)
		if err != nil {
			t.Fatalf("%s: Auth() = %v", step, err)
		}
		if len(keys) != wantKeys {
			t.Errorf("%s: %v keys unlocked, want %v", step, len(keys), wantKeys)
		}
		if !slices.Equal(requests, wantRequests) {
			t.Errorf("%s: requests %q, want %q", step, requests, wantRequests)
		}
	}

	if m.MarkStale("bob", "k2") {
		t.Error("MarkStale() = true without a session, want false")
	}
	login("first login", 1, "POST /auth/refresh", "GET /users", "GET /addresses", "GET /users")

	addr.Keys = append(addr.Keys, lockedKey(t, "k2", "mbox"))
	login("new key, not marked", 1)

	if !m.MarkStale("bob", "k2") {
		t.Error("MarkStale() = false for a new key, want true")
	}
	if m.MarkStale("bob", "k2") {
		t.Error("MarkStale() = true for a key marked before, want false")
	}
	if _, _, _, err := m.Auth("bob", "wrong"); err == nil {
		t.Error("Auth() with a wrong password = nil")
	}
	login("stale", 2, "GET /users", "GET /addresses")
	login("after", 2)

	// A key the password doesn't open stays locked, and is news only once.
	addr.Keys = append(addr.Keys, lockedKey(t, "k3", "other"))
	if !m.MarkStale("bob", "k3") {
		t.Error("MarkStale() = false for a new key, want true")
	}
	login("a key that stays locked", 2, "GET /users", "GET /addresses")
	if m.MarkStale("bob", "k3") {
		t.Error("MarkStale() = true for a key that stayed locked, want false")
	}
	login("after it", 2)
}

// roundTripFunc answers HTTP requests in memory, starting no goroutine, so it
// works inside a synctest bubble.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// tokenAPI answers /auth/refresh by the refresh token sent: "spent" tokens
// with 10013, others with a session whose refresh token is "next". Every
// other request is refused, and all are recorded.
func tokenAPI(t *testing.T, requests *[]string, spent ...string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		body := `{"Code":2001,"Error":"refused"}`
		status := http.StatusUnprocessableEntity
		if r.URL.Path == "/auth/refresh" {
			var sent struct{ RefreshToken string }
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Error(err)
			}
			*requests = append(*requests, "refresh "+sent.RefreshToken)
			if slices.Contains(spent, sent.RefreshToken) {
				body = `{"Code":10013,"Error":"Invalid refresh token"}`
			} else {
				status, body = http.StatusOK, `{"Code":1000,"UID":"u","AccessToken":"a","RefreshToken":"next","ExpiresIn":3600}`
			}
		} else {
			*requests = append(*requests, r.Method+" "+r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
}

// A client's login renews the session last saved: session.json's when it
// has one this bridge password opens, else the auth.json record's own. A
// refresh token spent by another process (10013) is retried with the one
// that process stored since; only when none is left does it log in again
// with the login password.
// evidence: witnesses
func TestOpenSessionTriesStoredToken(t *testing.T) {
	for _, tc := range []struct {
		name         string
		saved        string // refresh token session.json holds at first, "" for none
		storedSince  string // one another process stores during the first refresh
		wantRequests []string
	}{
		{"no session saved: the record's token", "", "", []string{"refresh record", "GET /users"}},
		{"the session saved", "saved", "", []string{"refresh saved", "GET /users"}},
		{"spent, another process stored a new one", "old", "new", []string{"refresh old", "refresh new", "GET /users"}},
		{"spent, none newer", "old", "", []string{"refresh old", "POST /auth/info"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			secretKey, _, err := GeneratePassword()
			if err != nil {
				t.Fatal(err)
			}
			if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "record"}, "bob", secretKey); err != nil {
				t.Fatal(err)
			}
			record, id, err := readRecord("bob", secretKey)
			if err != nil {
				t.Fatal(err)
			}
			sessionKey, err := deriveSessionKey(secretKey, "bob")
			if err != nil {
				t.Fatal(err)
			}
			save := func(token string) {
				t.Helper()
				if err := saveSession(&protonmail.Auth{UID: "u", RefreshToken: token}, "bob", &sessionKey, id); err != nil {
					t.Fatal(err)
				}
			}
			if tc.saved != "" {
				save(tc.saved)
			}

			var requests []string
			api := tokenAPI(t, &requests, "old")
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := api(r)
				if tc.storedSince != "" && len(requests) == 1 {
					save(tc.storedSince)
				}
				return resp, err
			})
			c := &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
			// Fails after what is checked: GET /users and /auth/info are refused.
			if _, err := openSession(c, record, id, "bob", secretKey); err == nil {
				t.Error("openSession() = nil, want the refused request after")
			}
			if !slices.Equal(requests, tc.wantRequests) {
				t.Errorf("requests %q, want %q", requests, tc.wantRequests)
			}
		})
	}
}

// A running session renews with its refresh token alone and saves the new
// one in session.json. When Proton refuses the token for good, it logs in
// with no password, as it keeps none: the session is dead, its requests
// fail without asking Proton again, and its death is signalled once. A
// server error leaves it alive.
// evidence: witnesses
func TestRenewKeepsNoPassword(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	secretKey, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "record", LoginPassword: "pw"}, "bob", secretKey); err != nil {
		t.Fatal(err)
	}
	_, id, err := readRecord("bob", secretKey)
	if err != nil {
		t.Fatal(err)
	}
	sessionKey, err := deriveSessionKey(secretKey, "bob")
	if err != nil {
		t.Fatal(err)
	}

	var requests []string
	unavailable := false
	api := tokenAPI(t, &requests, "next")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if unavailable {
			requests = append(requests, "unavailable")
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"Code":503,"Error":"later"}`)), Request: r}, nil
		}
		return api(r)
	})
	m := NewManager(nil)
	s := &session{
		c:          &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}},
		died:       make(chan struct{}, 1),
		auth:       protonmail.Auth{UID: "u", RefreshToken: "first"},
		sessionKey: sessionKey,
		record:     id,
	}

	unavailable = true
	if err := m.renew(s, "bob"); err == nil || errors.Is(err, errSessionDead) {
		t.Errorf("renew() while Proton is unavailable = %v, want an error that isn't the session's end", err)
	}
	unavailable = false

	requests = nil
	if err := m.renew(s, "bob"); err != nil {
		t.Fatalf("renew() = %v", err)
	}
	if got := mustStoredSession(t, "bob", &sessionKey); got == nil || got.RefreshToken != "next" {
		t.Errorf("storedSession() = %+v, want the renewed one", got)
	}
	if s.auth.RefreshToken != "next" || !slices.Equal(requests, []string{"refresh first"}) {
		t.Errorf("after renew(): token %q, requests %q", s.auth.RefreshToken, requests)
	}

	if len(s.died) != 0 {
		t.Error("the session's death is signalled while it lives")
	}

	// "next" is refused: no /auth/info, no login.
	requests = nil
	if err := m.renew(s, "bob"); !errors.Is(err, errSessionDead) {
		t.Errorf("renew() with a refused token = %v, want errSessionDead", err)
	}
	if err := m.renew(s, "bob"); !errors.Is(err, errSessionDead) {
		t.Errorf("renew() of a dead session = %v, want errSessionDead", err)
	}
	if !slices.Equal(requests, []string{"refresh next"}) {
		t.Errorf("requests %q, want one refused refresh and nothing else", requests)
	}
	if len(s.died) != 1 {
		t.Errorf("%v signals of the session's death, want 1", len(s.died))
	}
}

// Once Proton has refused the stored password for want of a human (CAPTCHA,
// 2FA), logins of the account are refused at once with that answer and no
// request, however often a client retries; a wrong bridge password is still
// just unauthorized. A record `peroxide auth` stores anew is tried again.
// The refusal is set by hand: making one needs a whole SRP login.
// evidence: witnesses
func TestAuthRefusedUntilNewRecord(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	secretKey, password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPassword, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "record"}, "bob", secretKey); err != nil {
		t.Fatal(err)
	}
	_, id, err := readRecord("bob", secretKey)
	if err != nil {
		t.Fatal(err)
	}

	var requests []string
	m := NewManager(func() *protonmail.Client {
		return &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: tokenAPI(t, &requests)}}
	})
	needs := &needsAuthError{"needs a human"}
	m.refused["bob"] = refusal{record: id, err: needs}

	for range 3 {
		if _, _, _, err := m.Auth("bob", password); !errors.Is(err, needs) {
			t.Errorf("Auth() of a refused record = %v, want the refusal", err)
		}
	}
	if _, _, _, err := m.Auth("bob", otherPassword); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Auth() with a wrong password = %v, want ErrUnauthorized", err)
	}
	if len(requests) != 0 {
		t.Errorf("requests %q, want none", requests)
	}

	// `peroxide auth` again, even with the same key, seals a new record.
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "anew"}, "bob", secretKey); err != nil {
		t.Fatal(err)
	}
	// Fails after what is checked: GET /users is refused.
	if _, _, _, err := m.Auth("bob", password); err == nil || errors.Is(err, needs) {
		t.Errorf("Auth() of a new record = %v, want it tried", err)
	}
	if !slices.Equal(requests, []string{"refresh anew", "GET /users"}) {
		t.Errorf("requests %q, want the new record's session renewed", requests)
	}
	if _, ok := m.refused["bob"]; ok {
		t.Error("the refusal outlived the record it was for")
	}
}

// accountAPI answers what a client's login asks: /auth/refresh by the token
// sent (10013 for the spent ones, else a session whose refresh token is t1,
// t2, ... in turn), /users and /addresses with the account. Every other
// request is refused; all are recorded.
type accountAPI struct {
	t        *testing.T
	user     *protonmail.User
	addr     *protonmail.Address
	spent    map[string]bool
	issued   int
	requests []string
	// The Authorization header of the last request.
	authorization string
}

// newAccountAPI returns the API of bob. Its keys are made at the first
// request for them: tests that never unlock any skip the cost.
func newAccountAPI(t *testing.T) *accountAPI {
	return &accountAPI{t: t, spent: map[string]bool{}}
}

// account returns bob's user and address, made at the first call.
func (a *accountAPI) account() (*protonmail.User, *protonmail.Address) {
	if a.user == nil {
		a.user = &protonmail.User{ID: "uid", Name: "bob", Keys: []*protonmail.PrivateKey{lockedKey(a.t, "user", "mbox")}}
		a.addr = &protonmail.Address{ID: "a", Email: "bob@example.org", Keys: []*protonmail.PrivateKey{lockedKey(a.t, "k1", "mbox")}}
	}
	return a.user, a.addr
}

func (a *accountAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	a.authorization = r.Header.Get("Authorization")
	status := http.StatusOK
	var body any
	switch r.URL.Path {
	case "/auth/refresh":
		var sent struct{ RefreshToken string }
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			a.t.Error(err)
		}
		a.requests = append(a.requests, "refresh "+sent.RefreshToken)
		if a.spent[sent.RefreshToken] {
			status, body = http.StatusUnprocessableEntity, map[string]any{"Code": 10013, "Error": "Invalid refresh token"}
		} else {
			a.issued++
			body = map[string]any{"Code": 1000, "UID": "u", "AccessToken": fmt.Sprint("a", a.issued),
				"RefreshToken": fmt.Sprint("t", a.issued), "ExpiresIn": 3600}
		}
	case "/users":
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		user, _ := a.account()
		body = map[string]any{"Code": 1000, "User": user}
	case "/addresses":
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		_, addr := a.account()
		body = map[string]any{"Code": 1000, "Addresses": []*protonmail.Address{addr}}
	case "/auth":
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodDelete {
			body = map[string]any{"Code": 1000}
		} else {
			status, body = http.StatusUnprocessableEntity, map[string]any{"Code": 2001, "Error": "refused"}
		}
	default:
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		status, body = http.StatusUnprocessableEntity, map[string]any{"Code": 2001, "Error": "refused"}
	}
	b, err := json.Marshal(body)
	if err != nil {
		a.t.Error(err)
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(b)), Request: r}, nil
}

// newAccount stores a record for bob, its mailbox password "mbox" and
// refresh token "r", and returns its bridge password and a Manager whose
// clients ask api.
func newAccount(t *testing.T, api *accountAPI) (password string, m *Manager) {
	t.Helper()
	secretKey, password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "r", MailboxPassword: "mbox"}, "bob", secretKey); err != nil {
		t.Fatal(err)
	}
	return password, NewManager(func() *protonmail.Client {
		return &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: api}}
	})
}

// After `peroxide auth -remove`, or `peroxide auth` again, a login with the
// old bridge password, or with the new one, ends the session read from the
// old record, dead or alive: it is logged out at Proton, its clients are
// signalled, and its client sends no token any more. The old password is
// refused; the new one opens a new session. A login with neither password
// ends nothing and asks Proton nothing.
// evidence: witnesses
func TestAuthEndsSessionOfChangedRecord(t *testing.T) {
	for _, tc := range []struct {
		name        string
		removed     bool // else replaced
		dead        bool
		newPassword bool // else the old one logs in
	}{
		{"removed, old password", true, false, false},
		{"replaced, old password", false, false, false},
		{"replaced, new password", false, false, true},
		{"replaced while dead, new password", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			api := newAccountAPI(t)
			password, m := newAccount(t, api)
			c, _, _, err := m.Auth("bob", password)
			if err != nil {
				t.Fatalf("first Auth() = %v", err)
			}
			old := m.sessions["bob"]
			died := m.SessionDied("bob")
			if tc.dead {
				old.dead = true
			}

			newKey, newPassword, err := GeneratePassword()
			if err != nil {
				t.Fatal(err)
			}
			if tc.removed {
				err = DeleteUsername("bob")
			} else {
				err = EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "anew", MailboxPassword: "mbox"}, "bob", newKey)
			}
			if err != nil {
				t.Fatal(err)
			}

			_, wrong, err := GeneratePassword()
			if err != nil {
				t.Fatal(err)
			}
			api.requests = nil
			if _, _, _, err := m.Auth("bob", wrong); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("Auth() with a wrong password = %v, want ErrUnauthorized", err)
			}
			if len(api.requests) != 0 || len(died) != 0 || old.ended || m.sessions["bob"] != old {
				t.Fatalf("a wrong password: requests %q, %v signals, ended %v; want nothing done", api.requests, len(died), old.ended)
			}

			api.requests = nil
			var c2 *protonmail.Client
			if tc.newPassword {
				c2, _, _, err = m.Auth("bob", newPassword)
				if err != nil {
					t.Fatalf("Auth() with the new password = %v", err)
				}
				if c2 == c || len(api.requests) == 0 || api.requests[0] != "refresh anew" {
					t.Errorf("new login: same client %v, requests %q; want a new session from the new record", c2 == c, api.requests)
				}
			} else if _, _, _, err := m.Auth("bob", password); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("Auth() with the old password = %v, want ErrUnauthorized", err)
			}
			if !slices.Contains(api.requests, "DELETE /auth") {
				t.Errorf("requests %q, want the old session logged out", api.requests)
			}
			if len(died) != 1 {
				t.Errorf("%v signals to the old session's clients, want 1", len(died))
			}
			if !old.dead || !old.ended || old.privateKeys != nil {
				t.Error("the old session lives on, or keeps its keys")
			}
			if _, err := c.GetCurrentUser(); err != nil {
				t.Fatal(err)
			}
			if api.authorization != "" {
				t.Errorf("the old client still sends %q", api.authorization)
			}
			if _, _, _, err := m.Auth("bob", password); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("Auth() with the old password, again = %v, want ErrUnauthorized", err)
			}
			if tc.removed {
				if _, _, _, err := m.Auth("bob", newPassword); !errors.Is(err, ErrUnauthorized) {
					t.Errorf("Auth() without a record = %v, want ErrUnauthorized", err)
				}
			}
		})
	}
}

// A live session whose record is replaced is ended by its next renewal, with
// no login needed: the renewed tokens are not saved but logged out at
// Proton, the session's clients are signalled, and the old bridge password
// logs in no more.
// evidence: witnesses
func TestRenewEndsSessionOfChangedRecord(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	api := newAccountAPI(t)
	password, m := newAccount(t, api)
	if _, _, _, err := m.Auth("bob", password); err != nil {
		t.Fatalf("Auth() = %v", err)
	}
	s := m.sessions["bob"]
	newKey, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "anew"}, "bob", newKey); err != nil {
		t.Fatal(err)
	}

	api.requests = nil
	if err := m.renew(s, "bob"); !errors.Is(err, errSessionEnded) {
		t.Errorf("renew() = %v, want errSessionEnded", err)
	}
	if want := []string{"refresh t1", "DELETE /auth"}; !slices.Equal(api.requests, want) || api.authorization != "Bearer a2" {
		t.Errorf("requests %q, the last with %q; want %q, the logout with the renewed token", api.requests, api.authorization, want)
	}
	if !s.dead || len(s.died) != 1 || m.sessions["bob"] != nil {
		t.Errorf("after renew(): dead %v, %v signals, kept %v; want ended and signalled once", s.dead, len(s.died), m.sessions["bob"] != nil)
	}
	if got := mustStoredSession(t, "bob", &s.sessionKey); got != nil {
		t.Errorf("session.json holds %+v, want nothing for the old record", got)
	}
	if _, _, _, err := m.Auth("bob", password); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Auth() with the old password = %v, want ErrUnauthorized", err)
	}
}

// A refresh that finishes after a login ended its session (the record
// replaced) leaves the client without tokens, and logs out the tokens it
// got: an ended session never comes back to life.
// evidence: witnesses
func TestRenewAfterEndDoesNotRearm(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	api := newAccountAPI(t)
	password, m := newAccount(t, api)
	c, _, _, err := m.Auth("bob", password)
	if err != nil {
		t.Fatalf("Auth() = %v", err)
	}
	s := m.sessions["bob"]
	// The record is unchanged, so the refresh saves; the login's ending
	// comes between the save and the client being armed.
	m.endSession(s, "bob", nil)
	api.requests = nil
	m.mu.Lock()
	s.dead = false // as if renew had read it before the end
	m.mu.Unlock()
	if err := m.renew(s, "bob"); !errors.Is(err, errSessionEnded) {
		t.Errorf("renew() of an ended session = %v, want errSessionEnded", err)
	}
	if !slices.Contains(api.requests, "DELETE /auth") {
		t.Errorf("requests %q, want the refreshed tokens logged out", api.requests)
	}
	if _, err := c.GetCurrentUser(); err != nil {
		t.Fatal(err)
	}
	if api.authorization != "" {
		t.Errorf("the ended client sends %q, want no token", api.authorization)
	}
}

// A dead session (its refresh token spent) is opened again by the next login
// with its bridge password, from the token another process saved since: the
// same session, alive, its death signal taken back, and no password login.
// evidence: witnesses
func TestAuthRevivesDeadSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	api := newAccountAPI(t)
	password, m := newAccount(t, api)
	c, _, _, err := m.Auth("bob", password)
	if err != nil {
		t.Fatalf("Auth() = %v", err)
	}
	s := m.sessions["bob"]
	api.spent[s.auth.RefreshToken] = true
	if err := m.renew(s, "bob"); !errors.Is(err, errSessionDead) {
		t.Fatalf("renew() with a spent token = %v, want errSessionDead", err)
	}
	_, wrong, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Auth("bob", wrong); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Auth() of a dead session with a wrong password = %v, want ErrUnauthorized", err)
	}
	// What another peroxide process would have left.
	if err := saveSession(&protonmail.Auth{UID: "u", RefreshToken: "other"}, "bob", &s.sessionKey, s.record); err != nil {
		t.Fatal(err)
	}

	api.requests = nil
	c2, keys, _, err := m.Auth("bob", password)
	if err != nil {
		t.Fatalf("Auth() of a dead session = %v", err)
	}
	if c2 != c || len(keys) != 1 || m.sessions["bob"] != s {
		t.Error("the dead session was replaced, want it opened again")
	}
	if s.dead || len(s.died) != 0 {
		t.Errorf("after Auth(): dead %v, %v signals pending; want alive and none", s.dead, len(s.died))
	}
	if len(api.requests) == 0 || api.requests[0] != "refresh other" || slices.Contains(api.requests, "POST /auth/info") {
		t.Errorf("requests %q, want the saved token refreshed and no password login", api.requests)
	}
}

// A session.json that can't be written doesn't fail the login, whose refresh
// has spent the token saved before: the session just got is the only one
// left. A record removed during the login does fail it.
// evidence: witnesses
func TestOpenSessionSaveFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		removed bool // else session.json can't be written
	}{
		{"session.json unwritable", false},
		{"record removed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			secretKey, _, err := GeneratePassword()
			if err != nil {
				t.Fatal(err)
			}
			if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "r", MailboxPassword: "mbox"}, "bob", secretKey); err != nil {
				t.Fatal(err)
			}
			record, id, err := readRecord("bob", secretKey)
			if err != nil {
				t.Fatal(err)
			}
			if tc.removed {
				err = DeleteUsername("bob")
			} else {
				// A directory where the file should be: neither read nor
				// replaced.
				var p string
				if p, err = paths.Path(sessionFile); err == nil {
					err = os.Mkdir(p, 0o700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}

			c := &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: newAccountAPI(t)}}
			o, err := openSession(c, record, id, "bob", secretKey)
			if tc.removed {
				if !errors.Is(err, errSessionNotStored) {
					t.Errorf("openSession() = %v, want errSessionNotStored", err)
				}
				return
			}
			if err != nil || o.auth.RefreshToken != "t1" {
				t.Errorf("openSession() = %+v, %v; want the session just got", o, err)
			}
		})
	}
}

// A session's verifier takes its own bridge password only, and is none of
// the keys derived from the same secret key, nor the key itself.
// evidence: witnesses
func TestVerifier(t *testing.T) {
	key, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	v, err := deriveVerifier(key, "bob")
	if err != nil {
		t.Fatal(err)
	}
	s := &session{verifier: v}
	for _, tc := range []struct {
		name     string
		key      *[32]byte
		username string
		want     bool
	}{
		{"own key", key, "bob", true},
		{"another key", other, "bob", false},
		{"zero key", new([32]byte), "bob", false},
		{"own key, another username", key, "alice", false},
	} {
		if got, err := s.matches(tc.key, tc.username); err != nil || got != tc.want {
			t.Errorf("%v: matches() = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	sessionKey, err := deriveSessionKey(key, "bob")
	if err != nil {
		t.Fatal(err)
	}
	storeKey, err := deriveStoreKey(key, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if v == *key || v == sessionKey || v == [32]byte(storeKey) {
		t.Error("the verifier is the secret key or another key derived from it")
	}
}

// Only the form of a bridge password `peroxide auth` prints is taken: other
// spellings of the same key (the spare bits set, no padding, line breaks)
// and anything not 32 bytes are refused.
// evidence: witnesses
func TestDecodeBridgePassword(t *testing.T) {
	key, password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := decodeBridgePassword(password); !ok || *got != *key {
		t.Fatalf("decodeBridgePassword(%q) = %v, %v; want the key", password, got, ok)
	}
	// The last character before the padding carries 4 bits of the key and
	// 2 spare bits: setting one gives another spelling of the same key.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := strings.IndexByte(alphabet, password[42])
	spare := password[:42] + string(alphabet[last^1]) + "="
	if b, err := base64.StdEncoding.DecodeString(spare); err != nil || [32]byte(b) != *key {
		t.Fatalf("%q isn't another spelling of the key: %v", spare, err)
	}
	for _, tc := range []struct {
		name, password string
	}{
		{"empty", ""},
		{"spare bits set", spare},
		{"no padding", strings.TrimSuffix(password, "=")},
		{"line break", password[:20] + "\n" + password[20:]},
		{"url alphabet", "-" + password[1:]},
		{"31 bytes", base64.StdEncoding.EncodeToString(key[:31])},
		{"33 bytes", base64.StdEncoding.EncodeToString(append(key[:], 0))},
		{"not base64", strings.Repeat("*", 44)},
	} {
		if got, ok := decodeBridgePassword(tc.password); ok {
			t.Errorf("%v: decodeBridgePassword(%q) = %v, want refused", tc.name, tc.password, got)
		}
	}
}

// An auth.json that can't be read refuses the login as a wrong password
// would: the client, not yet authenticated, learns nothing of the file, and
// a running session is neither used nor ended.
// evidence: witnesses
func TestUnreadableRecordsRefuseLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	api := newAccountAPI(t)
	password, m := newAccount(t, api)
	if _, _, _, err := m.Auth("bob", password); err != nil {
		t.Fatalf("Auth() = %v", err)
	}
	s := m.sessions["bob"]
	p, err := paths.Path(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob", "alice"} {
		if _, _, _, err := m.Auth(name, password); err != ErrUnauthorized {
			t.Errorf("Auth(%q) = %v, want exactly ErrUnauthorized", name, err)
		}
	}
	if s.ended || m.sessions["bob"] != s {
		t.Error("the session was ended by a file that can't be read")
	}
}

// Logins leave no lock behind once done: a client trying usernames that
// don't exist doesn't grow the Manager.
// evidence: witnesses
func TestAuthLeavesNoLoginLock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	api := newAccountAPI(t)
	password, m := newAccount(t, api)
	for _, name := range []string{"nobody", "nobody else", "bob"} {
		// Only the locks left matter, not whether the login succeeded.
		_, _, _, _ = m.Auth(name, password)
	}
	if len(m.logins) != 0 {
		t.Errorf("%v login locks left, want none", len(m.logins))
	}
}

// Clients logging in to one account at the same time refresh its stored
// token one after the other, never together: a refresh token is spent by its
// first use, so a second refresh in flight would fail and log in again.
// evidence: witnesses
func TestAuthOneRefreshAtATime(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	secretKey, password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: "r"}, "bob", secretKey); err != nil {
		t.Fatal(err)
	}

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		inFlight, most := 0, 0
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			// In the bubble's clock: the other logins run until they block
			// before this one answers.
			time.Sleep(time.Second)
			mu.Lock()
			inFlight--
			mu.Unlock()
			// Refused, so that each login makes its own refresh.
			return &http.Response{
				StatusCode: http.StatusUnprocessableEntity,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"Code":2001,"Error":"refused"}`)),
				Request:    r,
			}, nil
		})
		m := NewManager(func() *protonmail.Client {
			return &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
		})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if _, _, _, err := m.Auth("bob", password); err == nil {
					t.Error("Auth() = nil, want the refused refresh")
				}
			})
		}
		wg.Wait()
		if most != 1 {
			t.Errorf("%v refreshes of one account in flight at once, want 1", most)
		}
	})
}
