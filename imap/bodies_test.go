package imap

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

func testLimiter() *downloadLimiter {
	return newDownloadLimiter(rand.New(rand.NewPCG(1, 2)))
}

// AIMD: each round of successful requests adds one slot, up to maxParallel; a
// throttled one halves the parallelism, down to minParallel.
// evidence: witnesses
func TestLimiterAIMD(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := testLimiter()
		ctx := t.Context()
		succeed := func(n int) {
			for range n {
				if err := l.acquire(ctx, 1); err != nil {
					t.Fatal(err)
				}
				l.release(1, false, 0)
			}
		}
		succeed(2) // one round of 2: 2 + 1/2 + 1/2.5
		if p, _ := l.state(); math.Abs(p-2.9) > 1e-9 {
			t.Errorf("parallelism %v after a round of successes from 2, want 2.9", p)
		}
		succeed(1000)
		if p, _ := l.state(); p != maxParallel {
			t.Errorf("parallelism %v after many successes, want %v", p, maxParallel)
		}

		for want := float64(maxParallel / 2); want >= 1; want /= 2 {
			if err := l.acquire(ctx, 1); err != nil {
				t.Fatal(err)
			}
			l.release(1, true, time.Second)
			if p, _ := l.state(); p != want {
				t.Errorf("parallelism %v after a throttled request, want %v", p, want)
			}
			time.Sleep(time.Second)
		}
		if err := l.acquire(ctx, 1); err != nil {
			t.Fatal(err)
		}
		l.release(1, true, time.Second)
		if p, _ := l.state(); p != minParallel {
			t.Errorf("parallelism %v, want at least %v", p, minParallel)
		}
	})
}

// A throttled request stops every request from starting for its Retry-After,
// or, with none, for a back-off growing from minCoolDown to maxCoolDown with
// jitter; successes during the cool-down don't raise the parallelism.
// evidence: witnesses
func TestLimiterCoolDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := testLimiter()
		ctx := t.Context()
		if err := l.acquire(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if err := l.acquire(ctx, 1); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		l.release(1, true, 7*time.Second)
		l.release(1, false, 0) // in the cool-down: no increase
		if p, _ := l.state(); p != 1 {
			t.Errorf("parallelism %v, want 1: halved, not raised during the cool-down", p)
		}
		if err := l.acquire(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 7*time.Second {
			t.Errorf("waited %v after a Retry-After of 7s", waited)
		}
		l.release(1, false, 0)

		// Without a Retry-After: up to minCoolDown, then twice as long, capped.
		for _, ceiling := range []time.Duration{minCoolDown, 2 * minCoolDown, 4 * minCoolDown} {
			if err := l.acquire(ctx, 1); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			l.release(1, true, 0)
			if err := l.acquire(ctx, 1); err != nil {
				t.Fatal(err)
			}
			waited := time.Since(start)
			if waited < ceiling/2 || waited > ceiling {
				t.Errorf("cool-down %v, want between %v and %v", waited, ceiling/2, ceiling)
			}
			l.release(1, true, time.Nanosecond) // keep the back-off growing
			time.Sleep(time.Nanosecond)
		}
	})
}

// A request bigger than the bytes left waits for the others, but one alone
// always starts, however big.
// evidence: witnesses
func TestLimiterBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := testLimiter()
		ctx := t.Context()
		if err := l.acquire(ctx, maxInFlightBytes+1); err != nil {
			t.Fatalf("acquire() of one big message alone = %v", err)
		}
		started := make(chan struct{})
		go func() {
			if err := l.acquire(ctx, 1); err != nil {
				t.Error(err)
			}
			close(started)
		}()
		synctest.Wait()
		select {
		case <-started:
			t.Fatal("a second request started beside one over the byte limit")
		default:
		}
		l.release(maxInFlightBytes+1, false, 0)
		<-started
		l.release(1, false, 0)
	})
}

// acquire gives up when its context ends.
func TestLimiterCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := testLimiter()
		if err := l.acquire(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		l.release(1, true, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		errs := make(chan error)
		go func() { errs <- l.acquire(ctx, 1) }()
		synctest.Wait()
		cancel()
		if err := <-errs; err == nil {
			t.Error("acquire() returned nil after its context was cancelled")
		}
	})
}

// The body download stores every message, newest first first, with no more
// requests in flight than Proton allows for long: a fake that answers 429
// with a Retry-After above 4 at once makes the parallelism settle at or below
// it, and no message is lost. A message in the failed set is skipped.
func TestDownloadBodiesBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		const allowed = 4
		var msgs []*protonmail.Message
		for i := range 60 {
			m := inboxMessage("m"+strconv.Itoa(i), 0)
			m.Time = protonmail.Timestamp(1_700_000_000 + i)
			msgs = append(msgs, m)
		}
		var mu sync.Mutex
		inFlight, throttled := 0, 0
		var order []string
		api := &fakeMetadata{t: t, msgs: msgs}
		api.full = func(r *http.Request, id string) (*http.Response, error) {
			mu.Lock()
			inFlight++
			over := inFlight > allowed
			if over {
				throttled++
			} else {
				order = append(order, id)
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				inFlight--
				mu.Unlock()
			}()
			time.Sleep(100 * time.Millisecond)
			if over {
				resp := jsonResponse(r, http.StatusTooManyRequests, `{"Code":2028,"Error":"Too many requests"}`)
				resp.Header.Set("Retry-After", "1")
				return resp, nil
			}
			return jsonResponse(r, http.StatusOK, `{"Code":1000,"Message":{"ID":"`+id+`","MIMEType":"text/plain","Body":"b"}}`), nil
		}
		u, _, _, _ := newDownloadTestUser(t, api)
		for _, m := range msgs {
			if _, err := u.db.CreateMessage(m); err != nil {
				t.Fatal(err)
			}
		}
		if err := u.db.AddFailed("m0"); err != nil {
			t.Fatal(err)
		}

		if done := u.downloadBodies(t.Context(), false); !done {
			t.Error("downloadBodies() = false, want every message tried stored")
		}
		missing, err := u.db.MissingBodies()
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 1 || missing[0].ID != "m0" {
			t.Errorf("missing bodies %v, want only the failed m0", missing)
		}
		if throttled == 0 {
			t.Error("never throttled: the test doesn't exercise the back-off")
		}
		// AIMD saw-tooths: it halves at a 429 and climbs back by about one
		// per round of 4 requests, so about one 429 per three rounds.
		if throttled > len(msgs)/3 {
			t.Errorf("%v requests throttled for %v messages: the parallelism didn't settle near %v", throttled, len(msgs), allowed)
		}
		// Two start together; either may reach the fake first.
		if first := slices.Sorted(slices.Values(order[:2])); !slices.Equal(first, []string{"m58", "m59"}) {
			t.Errorf("first stored %v, want the newest, m59 and m58", first)
		}
	})
}
