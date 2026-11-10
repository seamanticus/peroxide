package imap

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ProtonMail/go-crypto/openpgp"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// fullResponse is the API's answer to GET /messages/{id} for msg.
func fullResponse(t *testing.T, msg *protonmail.Message) fakeResponse {
	t.Helper()
	b, err := json.Marshal(struct {
		Code    int
		Message *protonmail.Message
	}{1000, msg})
	if err != nil {
		t.Fatal(err)
	}
	return fakeResponse{http.StatusOK, "application/json", string(b)}
}

// refresh runs refreshMessage on apiID and returns what it delivered.
func refresh(t *testing.T, u *user, apiID string) []imapbackend.Update {
	t.Helper()
	var ups []imapbackend.Update
	if err := u.refreshMessage(apiID, func(d []imapbackend.Update) { ups = append(ups, d...) }); err != nil {
		t.Fatalf("refreshMessage(%v) = %v", apiID, err)
	}
	return ups
}

// An edited draft, stored, gets new UIDs with its new body, as gluon gives
// them: clients are told EXPUNGE then EXISTS. One whose bytes come out the
// same keeps its UID; one not stored makes no request.
// evidence: witnesses
func TestRefreshMessage(t *testing.T) {
	draft := fetchTestMessage("a", 0)
	draft.Body = "first draft\r\n"
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{draft, fetchTestMessage("b", 0)})
	cacheFull(t, mbox, draft)
	before := uidsByID(t, mbox)
	u := mbox.u

	same := *draft
	api.gets["/messages/a"] = fullResponse(t, &same)
	if ups := refresh(t, u, "a"); len(ups) != 0 {
		t.Errorf("refreshMessage() of an unchanged message delivered %v, want nothing", ups)
	}
	if got := uidsByID(t, mbox); got["a"] != before["a"] {
		t.Errorf("UID %v after an unchanged refresh, want %v", got["a"], before["a"])
	}

	edited := *draft
	edited.Body = "second draft\r\n"
	api.gets["/messages/a"] = fullResponse(t, &edited)
	ups := refresh(t, u, "a")
	if len(ups) != 2 {
		t.Fatalf("refreshMessage() gave %v updates, want EXPUNGE and EXISTS", len(ups))
	}
	if exp, ok := ups[0].(*imapbackend.ExpungeUpdate); !ok || exp.SeqNum != 1 {
		t.Errorf("first update %#v, want EXPUNGE 1", ups[0])
	}
	if ex, ok := ups[1].(*imapbackend.MailboxUpdate); !ok || ex.MailboxStatus.Messages != 2 {
		t.Errorf("second update %#v, want EXISTS 2", ups[1])
	}
	after := uidsByID(t, mbox)
	if after["a"] <= before["b"] || after["b"] != before["b"] {
		t.Errorf("UIDs %v after an edit, want a's above b's %v and b's kept", after, before["b"])
	}
	if _, literal, err := u.db.Body("a"); err != nil || !strings.Contains(string(literal), "second draft") {
		t.Errorf("stored %q, %v; want the edited body", literal, err)
	}

	n := len(api.madeRequests())
	if ups := refresh(t, u, "b"); len(ups) != 0 {
		t.Errorf("refreshMessage() of an unstored message delivered %v, want nothing", ups)
	}
	if got := api.madeRequests(); len(got) != n {
		t.Errorf("refreshMessage() of an unstored message made requests %v", got[n:])
	}
}

// The request of a refresh holds no lock: while it hangs, syncSem is free
// (an event page or a STORE goes ahead). A refresh whose message is dropped
// meanwhile (a later edit) renews nothing.
func TestRefreshHoldsNoLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		draft := inboxMessage("a", 0)
		hang, release := make(chan struct{}), make(chan struct{})
		api := &fakeMetadata{t: t, msgs: []*protonmail.Message{draft}}
		api.full = func(r *http.Request, id string) (*http.Response, error) {
			close(hang)
			<-release
			return jsonResponse(r, http.StatusOK, `{"Code":1000,"Message":{"ID":"a","MIMEType":"text/plain","Body":"edited"}}`), nil
		}
		u, inbox, _, _ := newDownloadTestUser(t, api)
		syncTestMailbox(t, u.db, protonmail.LabelInbox, []*protonmail.Message{draft})
		built := builtMessage{literal: []byte("Subject: old\r\n\r\nold\r\n")}
		if _, stored, err := u.db.PutBody("a", built.literal, built.info); err != nil || !stored {
			t.Fatal(stored, err)
		}
		before := uidsByID(t, inbox)["a"]

		var delivered []imapbackend.Update
		done := make(chan error)
		go func() {
			done <- u.refreshMessage("a", func(ups []imapbackend.Update) { delivered = append(delivered, ups...) })
		}()
		<-hang
		u.lockSync() // would block forever if the request held it
		// A later edit, applied under syncSem, drops the message.
		if err := u.builds.drop(u.db, "a"); err != nil {
			t.Fatal(err)
		}
		u.unlockSync()
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("refreshMessage() = %v", err)
		}
		if len(delivered) != 0 || uidsByID(t, inbox)["a"] != before {
			t.Errorf("a stale refresh delivered %v and renewed the UID", delivered)
		}
	})
}

// Several updates of one message in a page (autosaves) rebuild it once, after
// the page is applied.
func TestEditsInAPageRefreshOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		draft := inboxMessage("a", 0)
		api := &fakeMetadata{t: t, msgs: []*protonmail.Message{draft}}
		u, inbox, updates, _ := newDownloadTestUser(t, api)
		syncTestMailbox(t, u.db, protonmail.LabelInbox, []*protonmail.Message{draft})
		if err := u.db.SetLastEventID("e0"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := u.storedMessage(t.Context(), "a"); err != nil {
			t.Fatal(err)
		}
		api.mu.Lock()
		api.fulls = 0
		api.msgs = []*protonmail.Message{{ID: "a", LabelIDs: []string{protonmail.LabelInbox}, Subject: "edited"}}
		api.mu.Unlock()
		before := uidsByID(t, inbox)["a"]

		edit := func() *protonmail.EventMessage {
			return &protonmail.EventMessage{ID: "a", Action: protonmail.EventUpdate, Updated: &protonmail.EventMessageUpdate{}}
		}
		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{ID: "e1", After: "e0", Messages: []*protonmail.EventMessage{edit(), edit(), edit()}}
		close(events)
		u.receiveEvents(updates, events)

		api.mu.Lock()
		fulls := api.fulls
		api.mu.Unlock()
		if fulls != 1 {
			t.Errorf("three edits in a page made %v requests, want 1", fulls)
		}
		if after := uidsByID(t, inbox)["a"]; after == before {
			t.Error("the edited message kept its UID")
		}
	})
}

// Messages stored with something left encrypted are built again once a key
// that opens them is unlocked, and get new UIDs; the keys are recorded, so
// the next login builds nothing.
// evidence: witnesses
func TestRefreshFallbacksOnNewKey(t *testing.T) {
	old, added := newKey(t, "old"), newKey(t, "added")
	msg := fetchTestMessage("a", 0)
	msg.Body = encryptBody(t, "to the added key\r\n", added)
	api := &fakeAPI{t: t, gets: map[string]fakeResponse{"/messages/a": fullResponse(t, msg)}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	u := mbox.u
	u.privateKeys = openpgp.EntityList{old}
	cacheFull(t, mbox, msg)
	if rec, err := u.db.BodyRecord("a"); err != nil || !rec.Fallback {
		t.Fatalf("BodyRecord() = %+v, %v; want a fallback", rec, err)
	}
	var delivered []imapbackend.Update
	deliver := func(ups []imapbackend.Update) { delivered = append(delivered, ups...) }
	u.refreshFallbacks(t.Context(), deliver) // records the keys; builds the same bytes
	if len(delivered) != 0 {
		t.Fatalf("first refreshFallbacks() delivered %v, want nothing: same keys, same bytes", delivered)
	}

	u.privateKeys = openpgp.EntityList{old, added}
	delivered = nil
	u.refreshFallbacks(t.Context(), deliver)
	if len(delivered) != 2 {
		t.Errorf("refreshFallbacks() delivered %v updates, want EXPUNGE and EXISTS", len(delivered))
	}
	rec, literal, err := u.db.Body("a")
	if err != nil || rec.Fallback || !strings.Contains(string(literal), "to the added key") {
		t.Errorf("stored %q (fallback %v), %v; want it decrypted", literal, rec != nil && rec.Fallback, err)
	}

	n := len(api.madeRequests())
	u.refreshFallbacks(t.Context(), deliver)
	if got := api.madeRequests(); len(got) != n {
		t.Errorf("refreshFallbacks() with the same keys made requests %v", got[n:])
	}
}
