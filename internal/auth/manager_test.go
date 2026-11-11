package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// lockedKey returns a new private key as Proton keeps it: armored, locked
// with passphrase.
func lockedKey(t *testing.T, id, passphrase string) *protonmail.PrivateKey {
	t.Helper()
	e, err := openpgp.NewEntity(id, "", id+"@example.org", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EncryptPrivateKeys([]byte(passphrase), nil); err != nil {
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
	if got := storedSession("bob", &sessionKey); got == nil || got.RefreshToken != "next" {
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
