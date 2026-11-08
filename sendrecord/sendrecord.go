// Package sendrecord remembers the messages peroxide's SMTP server sent, for
// a while, so that an IMAP client saving its copy of one in Sent (APPEND)
// finds the message Proton already filed there instead of importing a
// second, as proton-bridge's sendrecorder does.
package sendrecord

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Expiry is how long a send is remembered (bridge's SendEntryExpiry).
const Expiry = 30 * time.Minute

// Recorder holds the sends of every account. It is safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	entries map[key][]*entry
	nextID  uint64
}

// key is a message, by Hash, sent from an account (its login name).
type key struct {
	user, hash string
}

type entry struct {
	id  uint64
	to  []string
	exp time.Time
	// messageID is the ID of the sent message, "" while it is being sent.
	messageID string
	// done is closed when the send is over: sent, or failed and removed.
	// ended records it, so that an expired send is closed once.
	done  chan struct{}
	ended bool
}

func (e *entry) end() {
	if !e.ended {
		e.ended = true
		close(e.done)
	}
}

func New() *Recorder {
	return &Recorder{entries: make(map[key][]*entry)}
}

// Send is a send Start recorded, which its sender ends with Sent or Failed.
type Send struct {
	r     *Recorder
	k     key
	id    uint64
	ended bool
}

// Start records that user is sending the message of hash to the recipients
// to. Every send is recorded: two identical messages are sent twice.
func (r *Recorder) Start(user, hash string, to []string) *Send {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeExpired()
	r.nextID++
	k := key{user, hash}
	r.entries[k] = append(r.entries[k], &entry{
		id:   r.nextID,
		to:   slices.Clone(to),
		exp:  time.Now().Add(Expiry),
		done: make(chan struct{}),
	})
	return &Send{r: r, k: k, id: r.nextID}
}

// Sent records that the send succeeded, as the message of messageID.
func (s *Send) Sent(messageID string) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		panic("invariant: a send ended twice")
	}
	s.ended = true
	if e := s.r.find(s.k, s.id); e != nil {
		e.messageID = messageID
		e.end()
	}
}

// Failed forgets the send.
func (s *Send) Failed() {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		panic("invariant: a send ended twice")
	}
	s.ended = true
	entries := s.r.entries[s.k]
	i := slices.IndexFunc(entries, func(e *entry) bool { return e.id == s.id })
	if i < 0 {
		return // expired
	}
	entries[i].end()
	if entries = slices.Delete(entries, i, i+1); len(entries) == 0 {
		delete(s.r.entries, s.k)
	} else {
		s.r.entries[s.k] = entries
	}
}

// SentMessage returns the ID of the message user sent of hash to the
// recipients to (in any order), if there is one in the last Expiry. A send of
// it still under way is waited for until ctx ends: then, or if it failed, the
// next is looked at; none found is ("", false).
func (r *Recorder) SentMessage(ctx context.Context, user, hash string, to []string) (string, bool) {
	for {
		r.mu.Lock()
		r.removeExpired()
		var pending *entry
		for _, e := range r.entries[key{user, hash}] {
			if !sameRecipients(e.to, to) {
				continue
			}
			if e.messageID != "" {
				r.mu.Unlock()
				return e.messageID, true
			}
			if pending == nil {
				pending = e
			}
		}
		r.mu.Unlock()
		if pending == nil {
			return "", false
		}
		select {
		case <-pending.done:
		case <-ctx.Done():
			return "", false
		}
	}
}

func (r *Recorder) find(k key, id uint64) *entry {
	for _, e := range r.entries[k] {
		if e.id == id {
			return e
		}
	}
	return nil
}

// removeExpired drops the sends older than Expiry. A pending one is closed:
// its waiters look again and find it gone.
func (r *Recorder) removeExpired() {
	now := time.Now()
	for k, entries := range r.entries {
		entries = slices.DeleteFunc(entries, func(e *entry) bool {
			if !e.exp.Before(now) {
				return false
			}
			e.end()
			return true
		})
		if len(entries) == 0 {
			delete(r.entries, k)
		} else {
			r.entries[k] = entries
		}
	}
}

// sameRecipients reports whether a and b hold the same addresses, as bridge's
// matchToList compares them: exactly, in any order.
func sameRecipients(a, b []string) bool {
	return len(a) == len(b) &&
		!slices.ContainsFunc(a, func(s string) bool { return !slices.Contains(b, s) }) &&
		!slices.ContainsFunc(b, func(s string) bool { return !slices.Contains(a, s) })
}
