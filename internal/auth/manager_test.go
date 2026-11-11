package auth

import (
	"bytes"
	"encoding/json"
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

// A refresh token spent by another process (10013) is retried with the one
// that process stored in auth.json, before any login; only when auth.json
// holds the same spent token does it fall to logging in again.
// evidence: witnesses
func TestAuthenticateTriesStoredToken(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stored       string // refresh token auth.json holds
		wantRequests []string
	}{
		{"another process stored a new one", "new", []string{"refresh old", "refresh new", "GET /users"}},
		{"the same one is stored", "old", []string{"refresh old", "POST /auth/info"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			secretKey, _, err := GeneratePassword()
			if err != nil {
				t.Fatal(err)
			}
			if err := EncryptAndSave(&CachedAuth{UID: "u", RefreshToken: tc.stored}, "bob", secretKey); err != nil {
				t.Fatal(err)
			}

			var requests []string
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"Code":2001,"Error":"refused"}`
				status := http.StatusUnprocessableEntity
				if r.URL.Path == "/auth/refresh" {
					b, _ := io.ReadAll(r.Body)
					switch {
					case strings.Contains(string(b), `"RefreshToken":"old"`):
						requests = append(requests, "refresh old")
						body = `{"Code":10013,"Error":"Invalid refresh token"}`
					case strings.Contains(string(b), `"RefreshToken":"new"`):
						requests = append(requests, "refresh new")
						status, body = http.StatusOK, `{"Code":1000,"UID":"u","AccessToken":"a","RefreshToken":"next","ExpiresIn":3600}`
					default:
						t.Errorf("refresh with %s", b)
					}
				} else {
					requests = append(requests, r.Method+" "+r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			c := &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
			cached := &CachedAuth{UID: "u", RefreshToken: "old"}
			// Fails after what is checked: GET /users and /auth/info are refused.
			if _, err := authenticate(c, cached, "bob", secretKey); err == nil {
				t.Error("authenticate() = nil, want the refused request after")
			}
			if !slices.Equal(requests, tc.wantRequests) {
				t.Errorf("requests %q, want %q", requests, tc.wantRequests)
			}
		})
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
