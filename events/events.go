package events

import (
	"log"
	"slices"
	"sync"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

// pollInterval is how often events are polled while a client is logged in.
// proton-bridge waits 20 s plus up to 20 s of jitter.
const pollInterval = 20 * time.Second

// EventSource gives the events after an event ID, one page at a time.
type EventSource interface {
	GetEvent(last string) (*protonmail.Event, error)
}

// Receiver polls the events of one Proton account and hands every page, in
// order, to each channel registered for it. It runs while one is registered.
type Receiver struct {
	c EventSource

	locker   sync.Mutex
	channels []chan<- *protonmail.Event

	poll chan struct{} // capacity 1: a pending request covers new ones
	stop chan struct{} // closed with the last channel
}

func (r *Receiver) receiveEvents(last string) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()

	for {
		last = r.receivePages(last)

		select {
		case <-t.C:
		case <-r.poll:
		case <-r.stop:
			return
		}
	}
}

// receivePages delivers the events after last, page after page until Proton
// has no more, and returns the ID of the last page delivered. A change
// spanning many pages is thus applied at once, not one page per poll.
func (r *Receiver) receivePages(last string) string {
	for {
		select {
		case <-r.stop:
			return last
		default:
		}

		event, err := r.c.GetEvent(last)
		if err != nil {
			log.Printf("cannot receive events after %v: %v", last, err)
			return last
		}

		r.locker.Lock()
		for _, ch := range r.channels {
			ch <- event
		}
		r.locker.Unlock()
		last = event.ID

		if event.More != 1 {
			return last
		}
	}
}

// Poll asks for the events now. It doesn't wait, even if r has stopped.
func (r *Receiver) Poll() {
	select {
	case r.poll <- struct{}{}:
	default:
	}
}

type Manager struct {
	receivers map[string]*Receiver
	locker    sync.Mutex
}

func NewManager() *Manager {
	return &Manager{
		receivers: make(map[string]*Receiver),
	}
}

// Register adds ch to the receiver of username, and starts one polling the
// events after lastEventID if none runs. Once done is closed, ch is removed
// and closed; the receiver stops with its last channel, so nothing is polled
// while no client is logged in.
func (m *Manager) Register(c EventSource, username, lastEventID string, ch chan<- *protonmail.Event, done <-chan struct{}) *Receiver {
	m.locker.Lock()
	defer m.locker.Unlock()

	r, ok := m.receivers[username]
	if ok {
		r.locker.Lock()
		r.channels = append(r.channels, ch)
		r.locker.Unlock()
	} else {
		r = &Receiver{
			c:        c,
			channels: []chan<- *protonmail.Event{ch},
			poll:     make(chan struct{}, 1),
			stop:     make(chan struct{}),
		}
		go r.receiveEvents(lastEventID)
		m.receivers[username] = r
	}

	go func() {
		<-done
		m.remove(username, r, ch)
	}()

	return r
}

// remove takes ch from r, and stops r if it was the last channel. Both happen
// under m.locker, so Register never adds a channel to a stopped receiver.
func (m *Manager) remove(username string, r *Receiver, ch chan<- *protonmail.Event) {
	m.locker.Lock()
	defer m.locker.Unlock()
	r.locker.Lock()
	defer r.locker.Unlock()

	r.channels = slices.DeleteFunc(r.channels, func(c chan<- *protonmail.Event) bool { return c == ch })
	close(ch)

	if len(r.channels) == 0 {
		close(r.stop)
		delete(m.receivers, username)
	}
}
