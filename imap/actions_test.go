package imap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/protonmail"
)

// fakeAPI answers message actions with a result per message: codes holds the
// code of each message, 1000 when missing. It records the requests made.
type fakeAPI struct {
	t     *testing.T
	codes map[string]int

	mu       sync.Mutex
	requests []string
}

func (api *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	api.requests = append(api.requests, r.Method+" "+r.URL.Path)
	api.mu.Unlock()

	if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/messages/") {
		api.t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	var req struct{ IDs []string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.t.Errorf("decode request: %v", err)
	}

	var results []string
	for _, id := range req.IDs {
		code, ok := api.codes[id]
		if !ok {
			code = 1000
		}
		res := fmt.Sprintf(`{"Code":%v}`, code)
		if code != 1000 {
			res = fmt.Sprintf(`{"Code":%v,"Details":[],"Error":"failed"}`, code)
		}
		results = append(results, fmt.Sprintf(`{"ID":%q,"Response":%v}`, id, res))
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"Code":1001,"Responses":[`+strings.Join(results, ",")+`]}`)
}

func (api *fakeAPI) madeRequests() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]string(nil), api.requests...)
}

// newActionTestMailbox returns a synchronized INBOX holding msgs, whose user
// talks to api, and the updates sent to clients. Updates are acknowledged as
// the IMAP server does once they are written.
func newActionTestMailbox(t *testing.T, api *fakeAPI, msgs []*protonmail.Message) (*mailbox, func() []imapbackend.Update) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	mbox, _ := newTestMailbox(t, msgs)
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: mbox}
	u.backend = &backend{updates: make(chan imapbackend.Update)}

	var mu sync.Mutex
	var sent []imapbackend.Update
	done := make(chan struct{})
	go func() {
		defer close(done)
		for update := range u.backend.updates {
			mu.Lock()
			sent = append(sent, update)
			mu.Unlock()
			close(update.Done())
		}
	}()
	t.Cleanup(func() {
		close(u.backend.updates)
		<-done
	})

	return mbox, func() []imapbackend.Update {
		mu.Lock()
		defer mu.Unlock()
		return append([]imapbackend.Update(nil), sent...)
	}
}

func uidSet(uids ...uint32) *imap.SeqSet {
	s := new(imap.SeqSet)
	s.AddNum(uids...)
	return s
}

// A read toggle is applied without waiting for Proton's event, and clients are
// told with a FETCH bearing the UID.
func TestStoreSeenAppliedWithoutEvent(t *testing.T) {
	api := &fakeAPI{t: t}
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 0), inboxMessage("b", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(2), imap.AddFlags, []string{imap.SeenFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}

	checkStatus(t, mbox, 2, 0)
	if got, want := api.madeRequests(), []string{"PUT /messages/read"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", got, want)
	}

	updates := sent()
	if len(updates) != 1 {
		t.Fatalf("sent %v updates, want 1", len(updates))
	}
	up, ok := updates[0].(*imapbackend.MessageUpdate)
	if !ok {
		t.Fatalf("sent %T, want a message update", updates[0])
	}
	if up.Message.SeqNum != 2 || up.Message.Uid != 2 || len(up.Message.Flags) != 1 || up.Message.Flags[0] != imap.SeenFlag {
		t.Errorf("sent FETCH %v UID %v FLAGS %v, want 2 UID 2 FLAGS [\\Seen]", up.Message.SeqNum, up.Message.Uid, up.Message.Flags)
	}
	if _, ok := up.Message.Items[imap.FetchUid]; !ok {
		t.Error("sent FETCH doesn't hold the UID item")
	}
}

// Proton reports a message already as asked as "does not exist": clients must
// still learn its flags, or a client toggling flags keeps asking the same.
func TestStoreSeenNothingToDo(t *testing.T) {
	api := &fakeAPI{t: t, codes: map[string]int{"a": protonmail.CodeMessageDoesNotExist}}
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(1), imap.RemoveFlags, []string{imap.SeenFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}
	checkStatus(t, mbox, 1, 1)
	if len(sent()) != 1 {
		t.Errorf("sent %v updates, want 1", len(sent()))
	}
}

// Messages the action failed for are left alone, and the command fails.
func TestStoreSeenFailure(t *testing.T) {
	api := &fakeAPI{t: t, codes: map[string]int{"b": 2011}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(1, 2), imap.AddFlags, []string{imap.SeenFlag}); err == nil {
		t.Error("UpdateMessagesFlags() = nil, want an error")
	}
	// a was marked read, b wasn't.
	checkStatus(t, mbox, 2, 1)
}

func TestMoveAppliedWithoutEvent(t *testing.T) {
	api := &fakeAPI{t: t}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 0)})
	archiveDB := syncTestMailbox(t, mbox.u.db, protonmail.LabelArchive, nil)
	archive := &mailbox{name: "Archive", label: protonmail.LabelArchive, u: mbox.u, db: archiveDB, deleted: make(map[string]struct{}), initialized: true}
	mbox.u.mailboxes[protonmail.LabelArchive] = archive

	if err := mbox.MoveMessages(true, uidSet(1), "Archive"); err != nil {
		t.Fatalf("MoveMessages() = %v", err)
	}
	checkStatus(t, mbox, 1, 0)
	checkStatus(t, archive, 1, 1)
	if got := len(api.madeRequests()); got != 2 {
		t.Errorf("made %v requests, want 2: label and unlabel", got)
	}
}

// EXPUNGE deletes the messages marked deleted once: they are gone after.
func TestExpungeForgetsDeletedMessages(t *testing.T) {
	api := &fakeAPI{t: t}
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 0), inboxMessage("b", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}
	if err := mbox.Expunge(); err != nil {
		t.Fatalf("Expunge() = %v", err)
	}
	checkStatus(t, mbox, 1, 1)
	if updates := sent(); len(updates) != 1 {
		t.Errorf("sent %v updates, want 1 EXPUNGE", len(updates))
	} else if up, ok := updates[0].(*imapbackend.ExpungeUpdate); !ok || up.SeqNum != 1 {
		t.Errorf("sent %#v, want EXPUNGE 1", updates[0])
	}

	if err := mbox.Expunge(); err != nil {
		t.Fatalf("second Expunge() = %v", err)
	}
	if got := len(api.madeRequests()); got != 1 {
		t.Errorf("made %v requests, want 1: the second EXPUNGE has nothing to delete", got)
	}
}
