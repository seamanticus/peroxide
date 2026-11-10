package auth

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

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
