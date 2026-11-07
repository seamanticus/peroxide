package imap

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// A second build of a message waits for the first: start makes one owner,
// and finish releases the others and lets the next build start.
// evidence: witnesses
func TestBuildsOneAtATime(t *testing.T) {
	var b builds
	bd, owner := b.start("a")
	if !owner {
		t.Fatal("first start() is not the owner")
	}
	waited, owner := b.start("a")
	if owner || waited != bd {
		t.Fatal("second start() of the same message is an owner, or waits on another build")
	}
	if _, owner := b.start("b"); !owner {
		t.Error("start() of another message waits")
	}
	b.finish("a")
	select {
	case <-waited.done:
	default:
		t.Error("finish() didn't release the waiting build")
	}
	if _, owner := b.start("a"); !owner {
		t.Error("start() after finish() is not the owner")
	}
}

// A build running when its message is dropped (an update event) was made
// from what Proton had before: it isn't stored, and the next build is.
// evidence: witnesses
func TestDroppedBuildNotStored(t *testing.T) {
	mbox, db := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0)})
	u := mbox.u
	built := builtMessage{literal: []byte("Subject: old\r\n\r\nold\r\n")}

	bd, _ := u.builds.start("a")
	if err := u.builds.drop(db, "a"); err != nil {
		t.Fatalf("drop() = %v", err)
	}
	if _, stored, err := u.builds.store(db, "a", bd, built); err != nil || stored {
		t.Errorf("store() of a dropped build = %v, %v; want not stored", stored, err)
	}
	u.builds.finish("a")
	if _, _, err := db.Body("a"); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("Body() = %v, want ErrNotFound", err)
	}

	bd, _ = u.builds.start("a")
	if _, stored, err := u.builds.store(db, "a", bd, built); err != nil || !stored {
		t.Errorf("store() of a new build = %v, %v; want stored", stored, err)
	}
	u.builds.finish("a")
}

// A message Proton refuses for a lasting reason (a 4xx other than 2501 or
// 429) goes in the failed set, and the FETCH fails; a rate limit doesn't;
// a FETCH that then succeeds takes it out.
// evidence: witnesses
func TestFailedMessagesRecorded(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s := status
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if s != http.StatusOK {
			w.WriteHeader(s)
			w.Write([]byte(`{"Code":2001,"Error":"refused"}`))
			return
		}
		w.Write([]byte(`{"Code":1000,"Message":{"ID":"a","Body":"body","MIMEType":"text/plain"}}`))
	}))
	t.Cleanup(srv.Close)
	setStatus := func(s int) {
		mu.Lock()
		status = s
		mu.Unlock()
	}

	mbox, db := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0)})
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}
	failed := func() bool {
		t.Helper()
		f, err := db.Failed("a")
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	setStatus(http.StatusTooManyRequests)
	if _, _, err := u.storedMessage(t.Context(), "a"); err == nil {
		t.Fatal("storedMessage() on 429 succeeded")
	}
	if failed() {
		t.Error("a rate-limited message is in the failed set")
	}

	setStatus(http.StatusBadRequest)
	if _, _, err := u.storedMessage(t.Context(), "a"); err == nil {
		t.Fatal("storedMessage() on 400 succeeded")
	}
	if !failed() {
		t.Error("a refused message isn't in the failed set")
	}

	setStatus(http.StatusOK)
	if _, _, err := u.storedMessage(t.Context(), "a"); err != nil {
		t.Fatalf("storedMessage() = %v", err)
	}
	if failed() {
		t.Error("a stored message is still in the failed set")
	}
}

// A message is downloaded and built once, then served from the store, across
// sessions too; once deleted it is gone from the store, and Proton is asked
// again if a FETCH still wants it.
func TestStoredMessageBuiltOnce(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	fetched := func() int {
		mu.Lock()
		defer mu.Unlock()
		return gets
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages/a" {
			t.Errorf("unexpected request to %v", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		gets++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Code":1000,"Message":{"ID":"a","Body":"body","MIMEType":"text/plain"}}`))
	}))
	t.Cleanup(srv.Close)

	mbox, _ := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0)})
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}

	for range 2 {
		rec, literal, err := u.storedMessage(t.Context(), "a")
		if err != nil {
			t.Fatalf("storedMessage() = %v", err)
		}
		if !strings.Contains(string(literal), "body") || rec.Size != uint32(len(literal)) {
			t.Errorf("storedMessage() = %+v, %q; want the body, its size", rec, literal)
		}
	}
	if n := fetched(); n != 1 {
		t.Errorf("fetched the message %v times, want 1", n)
	}
	if _, _, err := u.db.Body("a"); err != nil {
		t.Errorf("Body() = %v, want the message stored", err)
	}

	if _, err := u.applyDelete("a"); err != nil {
		t.Fatalf("applyDelete() = %v", err)
	}
	if _, _, err := u.db.Body("a"); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("Body() after delete = %v, want ErrNotFound", err)
	}
	if _, _, err := u.storedMessage(t.Context(), "a"); err != nil {
		t.Fatalf("storedMessage() after delete = %v", err)
	}
	if n := fetched(); n != 2 {
		t.Errorf("fetched the message %v times, want 2: deleting it drops it", n)
	}
}
