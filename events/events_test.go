package events

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

// fakeSource serves pages of events: pages[id] is the page after id, unless
// errs[id] holds an error to answer instead. latest is the latest event ID.
type fakeSource struct {
	mu    sync.Mutex
	pages map[string]*protonmail.Event
	errs  map[string]error

	latest    string
	latestErr error

	asked []string
}

func (s *fakeSource) GetEvent(last string) (*protonmail.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, last)
	if err, ok := s.errs[last]; ok {
		return nil, err
	}
	if page, ok := s.pages[last]; ok {
		return page, nil
	}
	return &protonmail.Event{ID: last}, nil // nothing new
}

func (s *fakeSource) GetLatestEventID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "latest")
	return s.latest, s.latestErr
}

// setErr makes the source answer err for the events after id, or serve them
// again if err is nil.
func (s *fakeSource) setErr(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.errs, id)
		return
	}
	if s.errs == nil {
		s.errs = make(map[string]error)
	}
	s.errs[id] = err
}

func (s *fakeSource) askedFor() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// receive collects the IDs of the pages sent on ch until it is closed.
func receive(ch <-chan *protonmail.Event) <-chan []string {
	ids := make(chan []string, 1)
	go func() {
		var got []string
		for event := range ch {
			got = append(got, event.ID)
		}
		ids <- got
	}()
	return ids
}

// newTestManager returns a Manager whose receivers wait exactly pollInterval.
func newTestManager() *Manager {
	m := NewManager()
	m.newPollDelay = func() func() time.Duration {
		return func() time.Duration { return pollInterval }
	}
	return m
}

// Pages that say More are followed at once, from the ID registered, not one
// page per poll interval.
func TestReceiverFollowsMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{
			"stored": {ID: "e1", More: 1},
			"e1":     {ID: "e2", More: 1},
			"e2":     {ID: "e3"},
		}}
		m := newTestManager()
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		m.Register(src, "bob", "stored", ch, done)

		synctest.Wait()
		if got, want := src.askedFor(), []string{"stored", "e1", "e2"}; !slices.Equal(got, want) {
			t.Errorf("asked for the events after %v, want %v", got, want)
		}

		close(done)
		if got, want := <-ids, []string{"e1", "e2", "e3"}; !slices.Equal(got, want) {
			t.Errorf("sent pages %v, want %v", got, want)
		}
	})
}

// Polls happen every pollInterval while a channel is registered, and stop
// with the last one: nothing is polled while no client is logged in.
func TestReceiverStopsWithLastChannel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{}}
		m := newTestManager()
		ch1, ch2 := make(chan *protonmail.Event), make(chan *protonmail.Event)
		done1, done2 := make(chan struct{}), make(chan struct{})
		ids1, ids2 := receive(ch1), receive(ch2)
		r := m.Register(src, "bob", "e0", ch1, done1)
		if again := m.Register(src, "bob", "ignored", ch2, done2); again != r {
			t.Fatal("second Register started a second receiver")
		}

		time.Sleep(pollInterval)
		synctest.Wait()
		if got := len(src.askedFor()); got != 2 {
			t.Errorf("polled %v times in one interval, want 2", got)
		}

		close(done1)
		<-ids1
		time.Sleep(pollInterval)
		synctest.Wait()
		if got := len(src.askedFor()); got != 3 {
			t.Errorf("polled %v times with one channel left, want 3", got)
		}

		close(done2)
		<-ids2
		time.Sleep(10 * pollInterval)
		synctest.Wait()
		if got := len(src.askedFor()); got != 3 {
			t.Errorf("polled %v times after the last channel left, want 3", got)
		}

		// Poll on a stopped receiver is answered at once.
		for range 2 {
			if err := <-r.Poll(); !errors.Is(err, errStopped) {
				t.Errorf("Poll on a stopped receiver = %v, want %v", err, errStopped)
			}
		}
	})
}

// After the last channel left, Register starts a new receiver, which resumes
// from the ID it is given.
func TestRegisterAfterStopStartsAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{"e5": {ID: "e6"}}}
		m := newTestManager()

		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		first := m.Register(src, "bob", "e0", ch, done)
		synctest.Wait()
		close(done)
		<-ids

		ch = make(chan *protonmail.Event)
		done = make(chan struct{})
		ids = receive(ch)
		if second := m.Register(src, "bob", "e5", ch, done); second == first {
			t.Error("Register reused a stopped receiver")
		}
		synctest.Wait()
		close(done)
		if got, want := <-ids, []string{"e6"}; !slices.Equal(got, want) {
			t.Errorf("sent pages %v, want %v", got, want)
		}
	})
}

// Every wait draws a new delay, counted from the end of the last poll.
func TestReceiverDrawsEachDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{}}
		m := NewManager()
		delays := []time.Duration{25 * time.Second, 35 * time.Second}
		var drawn int // only the receiver's goroutine draws
		m.newPollDelay = func() func() time.Duration {
			return func() time.Duration {
				d := delays[drawn%len(delays)]
				drawn++
				return d
			}
		}
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		m.Register(src, "bob", "e0", ch, done)

		// The first poll is at once; then 25 s, then 35 s after it.
		steps := []struct {
			sleep     time.Duration
			wantPolls int
		}{
			{0, 1},
			{delays[0], 2},
			{delays[1] - time.Nanosecond, 2},
			{time.Nanosecond, 3},
		}
		for i, step := range steps {
			time.Sleep(step.sleep)
			synctest.Wait()
			if got := len(src.askedFor()); got != step.wantPolls {
				t.Errorf("step %v: polled %v times, want %v", i, got, step.wantPolls)
			}
		}

		close(done)
		<-ids
	})
}

// The waits lie in [pollInterval, pollInterval+pollJitter), vary, and depend
// on the seed alone.
// evidence: witnesses
func TestJitteredPollDelay(t *testing.T) {
	var seed [32]byte
	seed[0] = 1
	a, b := newJitteredPollDelay(seed), newJitteredPollDelay(seed)
	seen := make(map[time.Duration]bool)
	for i := range 100 {
		da, db := a(), b()
		if da != db {
			t.Fatalf("draw %v: %v and %v from the same seed", i, da, db)
		}
		if da < pollInterval || da >= pollInterval+pollJitter {
			t.Fatalf("draw %v: %v, want in [%v, %v)", i, da, pollInterval, pollInterval+pollJitter)
		}
		seen[da] = true
	}
	if len(seen) < 50 {
		t.Errorf("100 draws gave %v distinct waits, want them to vary", len(seen))
	}

	seed[0] = 2
	other := newJitteredPollDelay(seed)
	same := 0
	for range 100 {
		if a() == other() {
			same++
		}
	}
	if same > 5 {
		t.Errorf("two seeds drew %v equal waits out of 100", same)
	}
}

// Poll asks for the events before the interval elapses.
func TestReceiverPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{}}
		m := newTestManager()
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		r := m.Register(src, "bob", "e0", ch, done)
		synctest.Wait()

		r.Poll()
		synctest.Wait()
		if got := len(src.askedFor()); got != 2 {
			t.Errorf("polled %v times after Poll, want 2", got)
		}

		close(done)
		<-ids
	})
}

// receiveAll collects the pages sent on ch until it is closed.
func receiveAll(ch <-chan *protonmail.Event) <-chan []*protonmail.Event {
	pages := make(chan []*protonmail.Event, 1)
	go func() {
		var got []*protonmail.Event
		for event := range ch {
			got = append(got, event)
		}
		pages <- got
	}()
	return pages
}

// A poll that fails delivers nothing and tells whoever asked for it; the next
// one asks for the same events again, and delivers them once Proton answers.
func TestReceiverRetriesAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unreachable := errors.New("dial tcp: connection refused")
		src := &fakeSource{pages: map[string]*protonmail.Event{"e0": {ID: "e1"}}}
		src.setErr("e0", unreachable)
		m := newTestManager()
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		r := m.Register(src, "bob", "e0", ch, done)
		synctest.Wait()

		if err := <-r.Poll(); !errors.Is(err, unreachable) {
			t.Errorf("failed Poll = %v, want %v", err, unreachable)
		}

		src.setErr("e0", nil)
		if err := <-r.Poll(); err != nil {
			t.Errorf("Poll = %v, want nil", err)
		}
		if got, want := src.askedFor(), []string{"e0", "e0", "e0"}; !slices.Equal(got, want) {
			t.Errorf("asked for the events after %v, want %v", got, want)
		}

		close(done)
		if got, want := <-ids, []string{"e1"}; !slices.Equal(got, want) {
			t.Errorf("sent pages %v, want %v", got, want)
		}
	})
}

// An event ID Proton refuses isn't asked for again: the receiver starts from
// the latest event, with a page asking to rebuild everything, as the events
// in between are lost.
func TestReceiverRestartsAfterRefusedID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{"l1": {ID: "l2"}}, latest: "l1"}
		src.setErr("stored", &protonmail.APIError{Status: http.StatusUnprocessableEntity, Code: 2061, Message: "Invalid ID"})
		m := newTestManager()
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		pages := receiveAll(ch)
		r := m.Register(src, "bob", "stored", ch, done)
		synctest.Wait()

		if err := <-r.Poll(); err != nil {
			t.Errorf("Poll = %v, want nil", err)
		}
		if got, want := src.askedFor(), []string{"stored", "latest", "l1"}; !slices.Equal(got, want) {
			t.Errorf("asked for %v, want %v", got, want)
		}

		close(done)
		got := <-pages
		if len(got) != 2 || got[0].ID != "l1" || got[0].Refresh&protonmail.EventRefreshMail == 0 || got[1].ID != "l2" {
			t.Errorf("sent pages %+v, want l1 with a mail Refresh, then l2", got)
		}
	})
}

// If the latest event ID can't be had either, nothing is delivered, and the
// refused ID is asked for again at the next poll.
func TestReceiverRestartFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		refusal := &protonmail.APIError{Status: http.StatusBadRequest, Code: 2001, Message: "Invalid value"}
		unreachable := errors.New("dial tcp: connection refused")
		src := &fakeSource{latestErr: unreachable}
		src.setErr("stored", refusal)
		m := newTestManager()
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		r := m.Register(src, "bob", "stored", ch, done)
		synctest.Wait()

		err := <-r.Poll()
		if !errors.Is(err, unreachable) || !errors.Is(err, refusal) {
			t.Errorf("Poll = %v, want both %v and %v", err, refusal, unreachable)
		}
		if got, want := src.askedFor(), []string{"stored", "latest", "stored", "latest"}; !slices.Equal(got, want) {
			t.Errorf("asked for %v, want %v", got, want)
		}

		close(done)
		if got := <-ids; len(got) != 0 {
			t.Errorf("sent pages %v, want none", got)
		}
	})
}

// Only answers saying the ID itself is wrong restart from the latest event: a
// session to refresh, a rate limit or a server error is retried with the
// same ID, as the events after it are still there.
// evidence: witnesses
func TestRefused(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"network", errors.New("dial tcp: i/o timeout"), false},
		{"no status", &protonmail.APIError{Code: 2501, Message: "x"}, false},
		{"400", &protonmail.APIError{Status: http.StatusBadRequest}, true},
		{"401", &protonmail.APIError{Status: http.StatusUnauthorized}, false},
		{"404", &protonmail.APIError{Status: http.StatusNotFound}, true},
		{"422", &protonmail.APIError{Status: http.StatusUnprocessableEntity}, true},
		{"422 wrapped", fmt.Errorf("get event: %w", &protonmail.APIError{Status: http.StatusUnprocessableEntity}), true},
		{"429", &protonmail.APIError{Status: http.StatusTooManyRequests}, false},
		{"500", &protonmail.APIError{Status: http.StatusInternalServerError}, false},
		{"503", &protonmail.APIError{Status: http.StatusServiceUnavailable}, false},
	}
	for _, tt := range tests {
		if got := refused(tt.err); got != tt.want {
			t.Errorf("%v: refused(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// A rate limit is not a refusal: the same ID is asked for again, and the
// latest event ID never.
func TestReceiverRetriesAfterRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{latest: "never"}
		src.setErr("e0", &protonmail.APIError{Status: http.StatusTooManyRequests, Code: 2028, Message: "Too many requests"})
		m := newTestManager()
		ch := make(chan *protonmail.Event)
		done := make(chan struct{})
		ids := receive(ch)
		m.Register(src, "bob", "e0", ch, done)

		time.Sleep(pollInterval)
		synctest.Wait()
		if got, want := src.askedFor(), []string{"e0", "e0"}; !slices.Equal(got, want) {
			t.Errorf("asked for %v, want %v", got, want)
		}

		close(done)
		if got := <-ids; len(got) != 0 {
			t.Errorf("sent pages %v, want none", got)
		}
	})
}

// A Poll asked for just before the receiver stops is answered, by the poll it
// asked for or by the stop, and never left waiting.
func TestPollAnsweredWhenReceiverStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{"e0": {ID: "e1"}}}
		m := newTestManager()
		ch := make(chan *protonmail.Event) // never read until done
		done := make(chan struct{})
		r := m.Register(src, "bob", "e0", ch, done)
		synctest.Wait() // the receiver waits to deliver e1

		result := r.Poll()
		ids := receive(ch)
		close(done)
		<-ids
		synctest.Wait()
		if err := <-result; err != nil && !errors.Is(err, errStopped) {
			t.Errorf("Poll = %v, want nil or %v", err, errStopped)
		}
	})
}
