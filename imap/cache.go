package imap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/seamanticus/peroxide/imap/database"
)

// errMessageGone is the error of storedMessage for a message Proton no longer
// has (code 2501): its delete event is on its way.
var errMessageGone = errors.New("message deleted in Proton")

// builds makes one build of a message at a time: FETCHes wanting a message
// another is building wait for it, then read it from the store, rather than
// downloading it again. It also orders storing a build against dropping what
// is stored (an event saying the message changed): a build running when its
// message is dropped was made from what Proton had before, and isn't stored.
type builds struct {
	mu      sync.Mutex
	pending map[string]*build
}

type build struct {
	done  chan struct{} // closed when the build ends
	stale bool          // its message was dropped since it started
}

// start returns apiID's running build, and whether the caller is to make it:
// false if another build of it is running, which the caller waits for.
func (b *builds) start(apiID string) (bd *build, owner bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bd, ok := b.pending[apiID]; ok {
		return bd, false
	}
	if b.pending == nil {
		b.pending = make(map[string]*build)
	}
	bd = &build{done: make(chan struct{})}
	b.pending[apiID] = bd
	return bd, true
}

func (b *builds) finish(apiID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.pending[apiID].done)
	delete(b.pending, apiID)
}

// store stores built, the build bd of apiID, unless it went stale; stored
// is false then, or if the message is gone from the database.
func (b *builds) store(db *database.User, apiID string, bd *build, built builtMessage) (rec *database.BodyRecord, stored bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bd.stale {
		return nil, false, nil
	}
	return db.PutBody(apiID, built.literal, built.info)
}

// isStale reports whether bd's message was dropped since bd started.
func (b *builds) isStale(bd *build) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bd.stale
}

// drop removes what is stored of apiID, and makes a running build of it
// stale.
func (b *builds) drop(db *database.User, apiID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bd, ok := b.pending[apiID]; ok {
		bd.stale = true
	}
	return db.DeleteBody(apiID)
}

// storedMessage returns the message apiID as served, from the store, building
// and storing it first if it isn't stored. A message Proton no longer has is
// errMessageGone.
func (u *user) storedMessage(ctx context.Context, apiID string) (*database.BodyRecord, []byte, error) {
	for {
		rec, literal, err := u.db.Body(apiID)
		if !errors.Is(err, database.ErrNotFound) {
			return rec, literal, err
		}
		bd, owner := u.builds.start(apiID)
		if !owner {
			<-bd.done
			continue
		}
		rec, literal, err = u.buildAndStore(ctx, apiID, bd)
		u.builds.finish(apiID)
		return rec, literal, err
	}
}

// buildAndStore downloads, builds and stores the message apiID, as bd. A
// download or build that fails for a reason retrying soon won't change (not
// the network, a rate limit or a server error, retryable) puts the message
// in the failed set, which the background download skips; storing takes it
// out again.
func (u *user) buildAndStore(ctx context.Context, apiID string, bd *build) (*database.BodyRecord, []byte, error) {
	built, err := u.downloadAndBuild(ctx, apiID)
	if errors.Is(err, errMessageGone) {
		return nil, nil, err
	} else if err != nil {
		return nil, nil, u.recordFailure(apiID, err)
	}
	rec, stored, err := u.builds.store(u.db, apiID, bd, built)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot store message %v: %w", apiID, err)
	}
	if !stored {
		// Deleted or changed while it was built: served all the same, as
		// the FETCH asked for it before the event, and built again by the
		// next FETCH.
		rec = &database.BodyRecord{Size: uint32(len(built.literal)), BodyInfo: built.info}
	}
	return rec, built.literal, nil
}

// recordFailure puts apiID in the failed set if err, its download or build's,
// isn't retryable, and returns err.
func (u *user) recordFailure(apiID string, err error) error {
	if retryable(err) {
		return err
	}
	log.Printf("Message %v failed to download or build, and is left for a FETCH to try again: %v", apiID, err)
	if addErr := u.db.AddFailed(apiID); addErr != nil {
		return errors.Join(err, fmt.Errorf("cannot record message %v as failed: %w", apiID, addErr))
	}
	return err
}
