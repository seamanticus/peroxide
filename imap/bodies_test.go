package imap

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emersion/go-imap"
	imapserver "github.com/emersion/go-imap/server"

	"github.com/seamanticus/peroxide/imap/database"
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
				if err := l.acquire(ctx); err != nil {
					t.Fatal(err)
				}
				l.release(false, 0)
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
			if err := l.acquire(ctx); err != nil {
				t.Fatal(err)
			}
			l.release(true, time.Second)
			if p, _ := l.state(); p != want {
				t.Errorf("parallelism %v after a throttled request, want %v", p, want)
			}
			time.Sleep(time.Second)
		}
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
		l.release(true, time.Second)
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
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		l.release(true, 7*time.Second)
		l.release(false, 0) // in the cool-down: no increase
		if p, _ := l.state(); p != 1 {
			t.Errorf("parallelism %v, want 1: halved, not raised during the cool-down", p)
		}
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 7*time.Second {
			t.Errorf("waited %v after a Retry-After of 7s", waited)
		}
		l.release(false, 0)

		// Without a Retry-After: up to minCoolDown, then twice as long, capped.
		for _, ceiling := range []time.Duration{minCoolDown, 2 * minCoolDown, 4 * minCoolDown} {
			if err := l.acquire(ctx); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			l.release(true, 0)
			if err := l.acquire(ctx); err != nil {
				t.Fatal(err)
			}
			waited := time.Since(start)
			if waited < ceiling/2 || waited > ceiling {
				t.Errorf("cool-down %v, want between %v and %v", waited, ceiling/2, ceiling)
			}
			l.release(true, time.Nanosecond) // keep the back-off growing
			time.Sleep(time.Nanosecond)
		}
	})
}

// A build bigger than the bytes left waits for the others, but one alone
// always starts, however big; a waiting build gives up when its context
// ends.
// evidence: witnesses
func TestMemBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var b memBudget
		ctx := t.Context()
		if err := b.acquire(ctx, maxInFlightBytes+1); err != nil {
			t.Fatalf("acquire() of one big message alone = %v", err)
		}
		started := make(chan struct{})
		go func() {
			if err := b.acquire(ctx, 1); err != nil {
				t.Error(err)
			}
			close(started)
		}()
		synctest.Wait()
		select {
		case <-started:
			t.Fatal("a second build started beside one over the byte limit")
		default:
		}
		b.release(maxInFlightBytes + 1)
		<-started

		// 1 byte held: a build of the whole budget doesn't fit.
		cancelled, cancel := context.WithCancel(ctx)
		errs := make(chan error)
		go func() { errs <- b.acquire(cancelled, maxInFlightBytes) }()
		synctest.Wait()
		cancel()
		if err := <-errs; err == nil {
			t.Error("acquire() returned nil after its context was cancelled")
		}
		b.release(1)
		if err := b.acquire(ctx, maxInFlightBytes); err != nil {
			t.Errorf("acquire() of the whole budget, nothing held = %v", err)
		}
		b.release(maxInFlightBytes)
	})
}

// A selected mailbox's messages go first, each part keeping newest first;
// All Mail moves nothing.
// evidence: witnesses
func TestPrioritized(t *testing.T) {
	m := func(id string, labels ...string) database.MissingBody {
		return database.MissingBody{ID: id, LabelIDs: labels}
	}
	missing := []database.MissingBody{m("a", "0"), m("b", "x"), m("c", "0", "x"), m("d", "0")}
	ids := func(ms []database.MissingBody) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	if got := ids(prioritized(missing, "x")); !slices.Equal(got, []string{"b", "c", "a", "d"}) {
		t.Errorf("prioritized(x) = %v, want [b c a d]", got)
	}
	if got := ids(prioritized(missing, protonmail.LabelAllMail)); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("prioritized(All Mail) = %v, want the order unchanged", got)
	}
}

// selectConn is a connection that takes any response.
type selectConn struct {
	imapserver.Conn
	ctx *imapserver.Context
}

func (c *selectConn) Context() *imapserver.Context  { return c.ctx }
func (c *selectConn) WriteResp(imap.WriterTo) error { return nil }

// SELECT and EXAMINE through Extension open the mailbox as go-imap does, and
// make it the body download's priority; each selection counts, the same
// mailbox again included.
func TestSelectPrioritizes(t *testing.T) {
	mbox, _ := newTestMailbox(t, nil)
	u := mbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: mbox}
	u.nameMailboxes()
	for i, name := range []string{"SELECT", "EXAMINE"} {
		cmd := Extension{}.Command(name)()
		sel, ok := cmd.(*selectCommand)
		if !ok {
			t.Fatalf("Extension doesn't handle %v", name)
		}
		sel.Mailbox = "INBOX"
		conn := &selectConn{ctx: &imapserver.Context{User: u}}
		// go-imap ends a successful SELECT with the tagged OK as an error.
		err := sel.Handle(conn)
		status, ok := errors.AsType[*imap.ErrStatusResp](err)
		if !ok || status.Resp.Type != imap.StatusRespOk {
			t.Fatalf("%v = %v, want go-imap's tagged OK", name, err)
		}
		if conn.ctx.Mailbox == nil || conn.ctx.MailboxReadOnly != (name == "EXAMINE") {
			t.Errorf("%v selected %v, read-only %v", name, conn.ctx.Mailbox, conn.ctx.MailboxReadOnly)
		}
		if label, gen := u.priority(); label != protonmail.LabelInbox || gen != uint64(i+1) {
			t.Errorf("after %v priority = %q, %v; want INBOX, %v", name, label, gen, i+1)
		}
	}

	// A mailbox that doesn't exist: the SELECT fails, and nothing moves.
	sel := Extension{}.Command("SELECT")().(*selectCommand)
	sel.Mailbox = "Nowhere"
	if err := sel.Handle(&selectConn{ctx: &imapserver.Context{User: u}}); err == nil {
		t.Error("SELECT of a mailbox that doesn't exist succeeded")
	}
	if _, gen := u.priority(); gen != 2 {
		t.Errorf("a failed SELECT changed the priority (selection %v, want 2)", gen)
	}
}

// A FETCH of unstored messages builds them several at once, as the
// download does, rather than one round trip each in turn: with every
// request taking 100 ms, 8 messages take far less than 800 ms.
func TestFetchBuildsInParallel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		var msgs []*protonmail.Message
		for i := range 8 {
			msgs = append(msgs, inboxMessage("m"+strconv.Itoa(i), 0))
		}
		var mu sync.Mutex
		inFlight, most := 0, 0
		api := &fakeMetadata{t: t, msgs: msgs}
		api.full = func(r *http.Request, id string) (*http.Response, error) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return jsonResponse(r, http.StatusOK, `{"Code":1000,"Message":{"ID":"`+id+`","MIMEType":"text/plain","Body":"b"}}`), nil
		}
		u, inbox, _, _ := newDownloadTestUser(t, api)
		syncTestMailbox(t, u.db, protonmail.LabelInbox, msgs)

		start := time.Now()
		got, err := fetchAll(t, inbox, imap.FetchRFC822Size)
		if err != nil || len(got) != len(msgs) {
			t.Fatalf("FETCH = %v messages, %v; want %v", len(got), err, len(msgs))
		}
		if took := time.Since(start); took >= 800*time.Millisecond {
			t.Errorf("FETCH of %v unstored messages took %v, want them built at once", len(msgs), took)
		}
		if most < 2 {
			t.Errorf("at most %v requests at once, want several", most)
		}
	})
}

// A FETCH of a message not stored yet waits for room in the account's
// memory budget, as the background download's builds do: no request is made
// until the build holding it ends.
func TestFetchBuildWaitsForMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		msg := inboxMessage("a", 0)
		msg.Size = 1 << 20
		api := &fakeMetadata{t: t, msgs: []*protonmail.Message{msg}}
		u, _, _, _ := newDownloadTestUser(t, api)
		if _, err := u.db.CreateMessage(msg); err != nil {
			t.Fatal(err)
		}
		// Another build holds all but less than the message.
		held := int64(maxInFlightBytes - msg.Size + 1)
		if err := u.mem.acquire(t.Context(), held); err != nil {
			t.Fatal(err)
		}

		done := make(chan error)
		go func() {
			_, _, err := u.storedMessage(t.Context(), "a")
			done <- err
		}()
		synctest.Wait()
		api.mu.Lock()
		fulls := api.fulls
		api.mu.Unlock()
		if fulls != 0 {
			t.Fatalf("made %v requests while the budget was full, want none", fulls)
		}
		u.mem.release(held)
		if err := <-done; err != nil {
			t.Errorf("storedMessage() = %v", err)
		}
	})
}

// acquire gives up when its context ends.
func TestLimiterCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := testLimiter()
		if err := l.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		l.release(true, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		errs := make(chan error)
		go func() { errs <- l.acquire(ctx) }()
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
