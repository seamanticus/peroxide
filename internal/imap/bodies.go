package imap

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/seamanticus/peroxide/internal/imap/database"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// pacer returns the account's request pacer, shared by the background
// download and FETCH, so that both slow down together when Proton asks to.
func (u *user) pacer() *downloadLimiter {
	u.pacerOnce.Do(func() {
		// Seeded from the account, so the jitter is the same in each run of
		// a test.
		u.pacerLimiter = newDownloadLimiter(rand.New(rand.NewPCG(uint64(len(u.username)), 1)))
	})
	return u.pacerLimiter
}

// downloadBodies downloads, builds and stores every message whose body isn't
// stored, newest first, as many at once as the limiter allows (TODO section
// 9 steps 2 and 3); the stored bodies are its progress, so it resumes where
// it stopped. Messages in the failed set are skipped, unless retryFailed. A
// request Proton throttles (429, 5xx) or the network fails is tried again
// after the limiter's cool-down; one that fails for another reason puts the
// message in the failed set (buildAndStore). It returns when every message
// was tried, or when ctx ends, and reports whether every message tried is
// stored.
//
// Requests hold no lock, and storing one message takes none of syncSem: a
// body changes nothing clients were told, so there is nothing to deliver. A
// FETCH wanting a message being downloaded waits for it (builds).
//
// A mailbox a client selects (prioritize) goes to the front: its messages
// not stored yet are taken next, still newest first, then the rest. So the
// mailbox in front of the user fills first, and its FETCHes rarely find a
// message to build.
func (u *user) downloadBodies(ctx context.Context, retryFailed bool) bool {
	missing, err := u.db.MissingBodies()
	if err != nil {
		log.Printf("cannot list the messages of %q to download: %v", u.u.Name, err)
		return false
	}
	if len(missing) == 0 {
		return true
	}
	log.Printf("Downloading %v messages of %q", len(missing), u.u.Name)

	limiter := u.pacer()
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored, failed := 0, 0
	var gen uint64
	for len(missing) > 0 {
		if label, g := u.priority(); g != gen {
			gen = g
			missing = prioritized(missing, label)
		}
		m := missing[0]
		missing = missing[1:]
		if !retryFailed {
			if f, err := u.db.Failed(m.ID); err != nil {
				log.Printf("cannot read whether message %v failed before: %v", m.ID, err)
				continue
			} else if f {
				continue
			}
		}
		if err := limiter.acquire(ctx); err != nil {
			break
		}
		wg.Go(func() {
			ok := u.downloadBody(ctx, limiter, m)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				stored++
			} else {
				failed++
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		log.Printf("Download of the messages of %q stopped: %v stored, %v to go", u.u.Name, stored, len(missing))
		return false
	}
	log.Printf("Downloaded %v messages of %q, %v failed", stored, u.u.Name, failed)
	return failed == 0
}

// prioritize makes the messages of the mailbox labelID the next the body
// download takes. Called when a client selects it.
func (u *user) prioritize(labelID string) {
	u.Lock()
	defer u.Unlock()
	u.priorityLabel = labelID
	u.priorityGen++
}

// priority returns the mailbox prioritize named last, and how many times it
// was called, so that the download sees a new selection of the same one.
func (u *user) priority() (labelID string, gen uint64) {
	u.Lock()
	defer u.Unlock()
	return u.priorityLabel, u.priorityGen
}

// prioritized returns missing with the messages of labelID first, each part
// in the order it had. All Mail holds every message: nothing moves.
func prioritized(missing []database.MissingBody, labelID string) []database.MissingBody {
	if labelID == protonmail.LabelAllMail {
		return missing
	}
	first := make([]database.MissingBody, 0, len(missing))
	var rest []database.MissingBody
	for _, m := range missing {
		if slices.Contains(m.LabelIDs, labelID) {
			first = append(first, m)
		} else {
			rest = append(rest, m)
		}
	}
	return append(first, rest...)
}

// downloadBody stores the message m, holding one of limiter's slots, which it
// releases. A throttled request is tried again once a slot is free after the
// cool-down. It reports whether m is stored, or gone from Proton or from the
// database.
func (u *user) downloadBody(ctx context.Context, limiter *downloadLimiter, m database.MissingBody) bool {
	for {
		_, _, err := u.storedMessage(ctx, m.ID)
		throttled := err != nil && protonmail.Retryable(err) && ctx.Err() == nil
		var after time.Duration
		if apiErr, ok := errors.AsType[*protonmail.APIError](err); ok {
			after = apiErr.RetryAfter
		}
		limiter.release(throttled, after)
		switch {
		case err == nil, errors.Is(err, errMessageGone), errors.Is(err, database.ErrNotFound):
			return true
		case !throttled:
			if ctx.Err() == nil {
				log.Printf("cannot download message %v: %v", m.ID, err)
			}
			return false
		}
		if err := limiter.acquire(ctx); err != nil {
			return false
		}
	}
}
