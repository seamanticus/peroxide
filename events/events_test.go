package events

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

// fakeSource serves pages of events: pages[id] is the page after id.
type fakeSource struct {
	mu    sync.Mutex
	pages map[string]*protonmail.Event
	asked []string
}

func (s *fakeSource) GetEvent(last string) (*protonmail.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, last)
	if page, ok := s.pages[last]; ok {
		return page, nil
	}
	return &protonmail.Event{ID: last}, nil // nothing new
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

// Pages that say More are followed at once, from the ID registered, not one
// page per poll interval.
func TestReceiverFollowsMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{
			"stored": {ID: "e1", More: 1},
			"e1":     {ID: "e2", More: 1},
			"e2":     {ID: "e3"},
		}}
		m := NewManager()
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
		m := NewManager()
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

		// Poll on a stopped receiver returns at once.
		r.Poll()
		r.Poll()
	})
}

// After the last channel left, Register starts a new receiver, which resumes
// from the ID it is given.
func TestRegisterAfterStopStartsAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{"e5": {ID: "e6"}}}
		m := NewManager()

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

// Poll asks for the events before the interval elapses.
func TestReceiverPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{pages: map[string]*protonmail.Event{}}
		m := NewManager()
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
