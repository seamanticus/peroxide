package imap

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/seamanticus/peroxide/protonmail"
)

func TestMessageCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newMessageCache()
	for i := 0; i < messageCacheSize; i++ {
		c.put(&protonmail.Message{ID: fmt.Sprint(i)})
	}
	// Using 0 makes 1 the least recently used.
	if _, ok := c.get("0"); !ok {
		t.Fatal("get(0) missed")
	}
	c.put(&protonmail.Message{ID: "new"})

	if _, ok := c.get("1"); ok {
		t.Error("get(1) hit, want it evicted")
	}
	for _, id := range []string{"0", "2", "new"} {
		if _, ok := c.get(id); !ok {
			t.Errorf("get(%v) missed", id)
		}
	}
}

// A message fetched once isn't fetched again until it's deleted.
func TestFullMessageCached(t *testing.T) {
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
		w.Write([]byte(`{"Code":1000,"Message":{"ID":"a","Body":"body"}}`))
	}))
	defer srv.Close()

	mbox, _ := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0)})
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: mbox}

	for i := 0; i < 2; i++ {
		msg, err := u.fullMessage("a")
		if err != nil {
			t.Fatalf("fullMessage() = %v", err)
		}
		if !strings.Contains(msg.Body, "body") {
			t.Errorf("fullMessage().Body = %q", msg.Body)
		}
	}
	if n := fetched(); n != 1 {
		t.Errorf("fetched the message %v times, want 1", n)
	}

	if _, err := u.applyDelete("a"); err != nil {
		t.Fatalf("applyDelete() = %v", err)
	}
	if _, err := u.fullMessage("a"); err != nil {
		t.Fatalf("fullMessage() after delete = %v", err)
	}
	if n := fetched(); n != 2 {
		t.Errorf("fetched the message %v times, want 2: deleting it drops it", n)
	}
}
