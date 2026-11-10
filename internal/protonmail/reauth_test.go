package protonmail

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// roundTripFunc answers HTTP requests in memory, starting no goroutine, so it
// works inside a synctest bubble.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// Requests that all get 401 with the same expired token renew the session
// once between them, as a refresh token is spent by its first use; each is
// then sent again with the new token and succeeds.
// evidence: witnesses
func TestReAuthOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") == "Bearer new" {
				return jsonResponse(r, http.StatusOK, `{"Code":1000}`), nil
			}
			// In the bubble's clock: every request is sent before any
			// gets its 401.
			time.Sleep(time.Second)
			return jsonResponse(r, http.StatusUnauthorized, `{"Code":401}`), nil
		})
		c := &Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
		c.setSession("uid", "old")
		reauths := 0
		c.ReAuth = func() error {
			reauths++ // serialized by reauth
			time.Sleep(time.Second)
			c.setSession("uid", "new")
			return nil
		}

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				req, err := c.newRequest(http.MethodGet, "/x", nil)
				if err != nil {
					t.Error(err)
					return
				}
				if err := c.doJSON(req, nil); err != nil {
					t.Errorf("request = %v", err)
				}
			})
		}
		wg.Wait()
		if reauths != 1 {
			t.Errorf("ReAuth ran %v times, want once", reauths)
		}
	})
}

// A 401 to a request ReAuth makes with the token it just got, marked
// withoutRenewal as Unlock's are, fails that request and the ReAuth: it
// doesn't wait for the ReAuth it is part of.
// evidence: witnesses
func TestReAuthInsideFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResponse(r, http.StatusUnauthorized, `{"Code":401}`), nil
		})
		c := &Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
		c.setSession("uid", "old")
		var inner error
		c.ReAuth = func() error {
			c.setSession("uid", "new")
			req, err := c.newRequest(http.MethodGet, "/users", nil)
			if err != nil {
				return err
			}
			inner = c.doJSON(withoutRenewal(req), nil)
			return inner
		}
		req, err := c.newRequest(http.MethodGet, "/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.doJSON(req, nil); err == nil {
			t.Error("request = nil, want the ReAuth's error")
		}
		if inner == nil {
			t.Error("request inside ReAuth = nil, want an error")
		}
	})
}

// A request that gets 401 again after the session was renewed fails: it
// renews once, not once per 401.
// evidence: witnesses
func TestReAuthRetriesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sent int
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent++
			return jsonResponse(r, http.StatusUnauthorized, `{"Code":401}`), nil
		})
		c := &Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
		c.setSession("uid", "t0")
		var reauths int
		c.ReAuth = func() error {
			reauths++
			c.setSession("uid", fmt.Sprint("t", reauths))
			return nil
		}
		req, err := c.newRequest(http.MethodGet, "/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.doJSON(req, nil); err == nil {
			t.Error("request = nil, want the second 401")
		}
		if reauths != 1 || sent != 2 {
			t.Errorf("ReAuth ran %v times and %v requests were sent, want 1 and 2", reauths, sent)
		}
	})
}

// A request another goroutine sends with the new token while ReAuth is still
// running (between its login and the end of Unlock) and that gets 401 waits
// for that ReAuth and is sent again, rather than failing as one inside it.
// evidence: witnesses
func TestReAuthNewTokenOutsideWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// "new" is refused until the ReAuth is done, as if Proton were
		// slow to accept it; then "newer" works.
		done := false
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") == "Bearer newer" {
				return jsonResponse(r, http.StatusOK, `{"Code":1000}`), nil
			}
			return jsonResponse(r, http.StatusUnauthorized, `{"Code":401}`), nil
		})
		c := &Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: transport}}
		c.setSession("uid", "old")
		set, release := make(chan struct{}), make(chan struct{})
		c.ReAuth = func() error {
			c.setSession("uid", "new")
			close(set)
			<-release
			c.setSession("uid", "newer")
			done = true
			return nil
		}

		var wg sync.WaitGroup
		wg.Go(func() {
			req, err := c.newRequest(http.MethodGet, "/first", nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := c.doJSON(req, nil); err != nil {
				t.Errorf("request setting off ReAuth = %v", err)
			}
		})
		<-set
		wg.Go(func() {
			req, err := c.newRequest(http.MethodGet, "/second", nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := c.doJSON(req, nil); err != nil {
				t.Errorf("request with the new token during ReAuth = %v, want it to wait", err)
			}
		})
		synctest.Wait()
		close(release)
		wg.Wait()
		if !done {
			t.Error("ReAuth didn't finish")
		}
	})
}
