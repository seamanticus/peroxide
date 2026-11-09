package auth

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

// roundTripFunc answers HTTP requests in memory, starting no goroutine, so it
// works inside a synctest bubble.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
