package imap

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// The body download's limits (TODO section 9 step 3, decided 2026-11-08).
const (
	// Parallel requests at the start, and at most: 32 is proton-bridge's
	// MaxParallelDownloads, "the maximum recommended by the API team".
	minParallel   = 1
	startParallel = 2
	maxParallel   = 32
	// maxInFlightBytes bounds the memory messages being downloaded and built
	// take, by Proton's sizes, as bridge's DownloadRequestMem.
	maxInFlightBytes = 64 << 20
	// The cool-down after a throttled request when Proton gives no
	// Retry-After: exponential, with jitter.
	minCoolDown = 2 * time.Second
	maxCoolDown = 60 * time.Second
)

// downloadLimiter paces the body download by AIMD, as TCP's congestion
// control: each request that succeeds while no cool-down runs adds
// 1/parallelism (so +1 per round of requests), up to maxParallel; a throttled
// one (429, 5xx, the network) halves it, at least to minParallel, and starts
// a cool-down, the Retry-After Proton gives or else an exponential back-off
// with jitter, during which no request starts. It also bounds the bytes in
// flight. Safe for concurrent use.
type downloadLimiter struct {
	mu        sync.Mutex
	parallel  float64   // requests allowed at once
	inFlight  int       // requests started and not done
	bytes     int64     // their sizes
	coolUntil time.Time // no request starts before
	backoff   time.Duration
	rng       *rand.Rand
	changed   chan struct{} // closed and replaced when a slot may be free
}

// newDownloadLimiter returns a limiter whose jitter is drawn from rng.
func newDownloadLimiter(rng *rand.Rand) *downloadLimiter {
	return &downloadLimiter{parallel: startParallel, backoff: minCoolDown, rng: rng, changed: make(chan struct{})}
}

// acquire waits until a request of size bytes may start, and counts it in
// flight: no cool-down runs, fewer than the parallelism are in flight, and
// its bytes fit, or nothing else is in flight. It fails only when ctx ends.
func (l *downloadLimiter) acquire(ctx context.Context, size int64) error {
	for {
		l.mu.Lock()
		wait := time.Until(l.coolUntil)
		if wait <= 0 && l.inFlight < int(l.parallel) && (l.inFlight == 0 || l.bytes+size <= maxInFlightBytes) {
			l.inFlight++
			l.bytes += size
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()

		var timer <-chan time.Time
		var t *time.Timer
		if wait > 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-changed:
		case <-timer:
		case <-ctx.Done():
		}
		if t != nil {
			t.Stop()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// release ends a request of size bytes acquire let start. throttled says
// Proton asked to slow down, with retryAfter its Retry-After (0 if none).
func (l *downloadLimiter) release(size int64, throttled bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight <= 0 {
		panic("invariant: downloadLimiter.release without acquire")
	}
	l.inFlight--
	l.bytes -= size
	now := time.Now()
	switch {
	case throttled:
		l.parallel = max(minParallel, l.parallel/2)
		d := retryAfter
		if d <= 0 {
			// Half the back-off, plus up to as much again at random.
			d = l.backoff/2 + time.Duration(l.rng.Int64N(int64(l.backoff/2)+1))
			l.backoff = min(maxCoolDown, 2*l.backoff)
		}
		l.coolUntil = now.Add(d)
	case !now.Before(l.coolUntil):
		l.parallel = min(maxParallel, l.parallel+1/l.parallel)
		l.backoff = minCoolDown
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

// state returns the parallelism and the requests in flight, for tests and
// logs.
func (l *downloadLimiter) state() (parallel float64, inFlight int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.parallel, l.inFlight
}
