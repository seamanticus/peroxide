package events

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

// Each wait between polls, while a client is logged in, lasts pollInterval
// plus a random part of pollJitter, as proton-bridge's: accounts and peroxide
// instances then don't poll Proton in step.
const (
	pollInterval = 20 * time.Second
	pollJitter   = 20 * time.Second
)

// newJitteredPollDelay returns the waits of one receiver, drawn from a
// ChaCha8 source seeded with seed. Only that receiver's goroutine calls it.
func newJitteredPollDelay(seed [32]byte) func() time.Duration {
	r := rand.New(rand.NewChaCha8(seed))
	return func() time.Duration {
		return pollInterval + time.Duration(r.Int64N(int64(pollJitter)))
	}
}

// randomSeed returns a seed from the operating system, so that two peroxide
// instances don't draw the same waits. crypto/rand.Read never fails.
func randomSeed() [32]byte {
	var seed [32]byte
	crand.Read(seed[:])
	return seed
}

// EventSource gives the events after an event ID, one page at a time, and the
// ID of the latest event.
type EventSource interface {
	GetEvent(last string) (*protonmail.Event, error)
	GetLatestEventID() (string, error)
}

// errStopped answers the polls asked of a receiver that has stopped.
var errStopped = errors.New("events: receiver stopped, no client is logged in")

// Receiver polls the events of one Proton account and hands every page, in
// order, to each channel registered for it. It runs while one is registered.
type Receiver struct {
	c     EventSource
	delay func() time.Duration // the wait before the next poll

	locker   sync.Mutex
	channels []chan<- *protonmail.Event

	poll chan struct{} // capacity 1: a pending request covers new ones
	stop chan struct{} // closed with the last channel

	// waiters get the outcome of the next poll to start after they asked
	// (Poll). A lock of their own, as r.locker is held while a page waits to
	// be delivered, and Poll mustn't wait for that.
	waitersMu sync.Mutex
	waiters   []chan<- error
	stopped   bool // set, under waitersMu, once r answers no more waiters
}

func (r *Receiver) receiveEvents(last string) {
	defer r.answerWaiters(errStopped, true)
	for {
		waiters := r.takeWaiters()
		var err error
		last, err = r.receivePages(last)
		for _, w := range waiters {
			w <- err // capacity 1, sent once
		}

		// Drawn afresh each time, and counted from the end of the last
		// poll, so an early Poll also postpones the next one.
		select {
		case <-time.After(r.delay()):
		case <-r.poll:
		case <-r.stop:
			return
		}
	}
}

func (r *Receiver) takeWaiters() []chan<- error {
	r.waitersMu.Lock()
	defer r.waitersMu.Unlock()
	waiters := r.waiters
	r.waiters = nil
	return waiters
}

// answerWaiters sends err to every waiter left; stop makes r answer every
// later Poll at once with errStopped.
func (r *Receiver) answerWaiters(err error, stop bool) {
	r.waitersMu.Lock()
	defer r.waitersMu.Unlock()
	for _, w := range r.waiters {
		w <- err
	}
	r.waiters = nil
	r.stopped = r.stopped || stop
}

// receivePages delivers the events after last, page after page until Proton
// has no more, and returns the ID of the last page delivered. A change
// spanning many pages is thus applied at once, not one page per poll. err is
// why it stopped early; last is then where the next poll starts again.
func (r *Receiver) receivePages(last string) (string, error) {
	for {
		select {
		case <-r.stop:
			return last, errStopped
		default:
		}

		event, err := r.c.GetEvent(last)
		if last != "" && refused(err) {
			event, err = r.restart(last, err)
		}
		if err != nil {
			log.Printf("cannot receive events after %v: %v", last, err)
			return last, err
		}

		r.locker.Lock()
		for _, ch := range r.channels {
			ch <- event
		}
		r.locker.Unlock()
		last = event.ID

		if event.More != 1 {
			return last, nil
		}
	}
}

// refused reports whether err is Proton refusing the event ID asked for, as
// opposed to a failure worth retrying with the same ID: a network error, a
// session to refresh (401), a rate limit (429) or a server error (5xx). What
// Proton answers for an ID it no longer keeps isn't recorded yet; these are
// the statuses it gives for IDs it doesn't know (proton-bridge,
// service_label_events.go and go-proton-api's server: 422 and 400).
func refused(err error) bool {
	apiErr, ok := errors.AsType[*protonmail.APIError](err)
	if !ok {
		return false
	}
	switch apiErr.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// restart gives up on the events after last, which Proton refused with
// refusal: events since then are lost, so the page it returns starts from the
// latest event and says to rebuild everything (Refresh), as Proton's own
// Refresh does. Without it, a stored ID Proton refuses would be asked for
// again at every poll, and every login, for good.
func (r *Receiver) restart(last string, refusal error) (*protonmail.Event, error) {
	latest, err := r.c.GetLatestEventID()
	if err != nil {
		return nil, errors.Join(refusal, fmt.Errorf("cannot get the latest event ID: %w", err))
	}
	log.Printf("Proton refused the events after %v (%v): starting again from event %v, and rebuilding every mailbox", last, refusal, latest)
	return &protonmail.Event{ID: latest, Refresh: protonmail.EventRefreshMail}, nil
}

// Poll asks for the events now. It doesn't wait: the channel returned gets
// the outcome of the next poll that starts after the call, nil once all its
// pages are delivered, else the error that stopped it, errStopped if r has
// stopped or stops first. The channel has room for it: callers may ignore it.
func (r *Receiver) Poll() <-chan error {
	result := make(chan error, 1)
	r.waitersMu.Lock()
	if r.stopped {
		result <- errStopped
	} else {
		r.waiters = append(r.waiters, result)
	}
	r.waitersMu.Unlock()

	select {
	case r.poll <- struct{}{}:
	default:
	}
	return result
}

type Manager struct {
	receivers map[string]*Receiver
	locker    sync.Mutex

	// newPollDelay gives each new Receiver its waits between polls. Tests
	// replace it.
	newPollDelay func() func() time.Duration
}

func NewManager() *Manager {
	return &Manager{
		receivers: make(map[string]*Receiver),
		newPollDelay: func() func() time.Duration {
			return newJitteredPollDelay(randomSeed())
		},
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
			delay:    m.newPollDelay(),
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
