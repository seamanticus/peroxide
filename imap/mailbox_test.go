package imap

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// openTestDB opens a database in a temporary directory.
func openTestDB(t *testing.T) *database.User {
	t.Helper()
	// os.UserConfigDir reads one of these, depending on the OS.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)

	db, err := database.Open("test.db")
	if err != nil {
		t.Fatalf("database.Open() = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close() = %v", err)
		}
	})
	return db
}

// syncTestMailbox synchronizes the mailbox labelID of db with msgs, the way
// mailbox.sync does.
func syncTestMailbox(t *testing.T, db *database.User, labelID string, msgs []*protonmail.Message) *database.Mailbox {
	t.Helper()
	mboxDB, err := db.Mailbox(labelID)
	if err != nil {
		t.Fatalf("db.Mailbox() = %v", err)
	}
	if err := mboxDB.Reset(); err != nil {
		t.Fatalf("Reset() = %v", err)
	}
	if err := mboxDB.Sync(msgs); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	return mboxDB
}

// newTestMailbox returns a synchronized INBOX backed by a temporary database,
// holding msgs.
func newTestMailbox(t *testing.T, msgs []*protonmail.Message) (*mailbox, *database.User) {
	t.Helper()
	db := openTestDB(t)
	mboxDB := syncTestMailbox(t, db, protonmail.LabelInbox, msgs)

	return &mailbox{
		name:        imap.InboxName,
		label:       protonmail.LabelInbox,
		u:           &user{db: db, u: &protonmail.User{Name: "jon"}},
		db:          mboxDB,
		deleted:     make(map[string]struct{}),
		initialized: true,
	}, db
}

func inboxMessage(id string, unread int) *protonmail.Message {
	return &protonmail.Message{ID: id, Unread: unread, LabelIDs: []string{protonmail.LabelInbox}}
}

func checkStatus(t *testing.T, mbox *mailbox, wantMessages, wantUnseen uint32) {
	t.Helper()
	status, err := mbox.Status([]imap.StatusItem{imap.StatusMessages, imap.StatusUnseen})
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if status.Messages != wantMessages || status.Unseen != wantUnseen {
		t.Errorf("Status() = MESSAGES %v UNSEEN %v, want MESSAGES %v UNSEEN %v",
			status.Messages, status.Unseen, wantMessages, wantUnseen)
	}
}

// Proton's events carry no message counts, so counts cached at login go stale:
// STATUS must follow the local database, which events keep up to date.
func TestStatusFollowsEvents(t *testing.T) {
	mbox, db := newTestMailbox(t, []*protonmail.Message{
		inboxMessage("a", 0),
		inboxMessage("b", 0),
	})
	// Counts from /messages/count at login.
	mbox.setAPICounts(2, 0)
	checkStatus(t, mbox, 2, 0)

	if _, err := db.CreateMessage(inboxMessage("c", 1)); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	checkStatus(t, mbox, 3, 1)

	read := 0
	if _, _, err := db.UpdateMessage("c", &protonmail.EventMessageUpdate{Unread: &read}); err != nil {
		t.Fatalf("UpdateMessage() = %v", err)
	}
	checkStatus(t, mbox, 3, 0)

	unread := 1
	if _, _, err := db.UpdateMessage("a", &protonmail.EventMessageUpdate{Unread: &unread}); err != nil {
		t.Fatalf("UpdateMessage() = %v", err)
	}
	checkStatus(t, mbox, 3, 1)

	if _, err := db.DeleteMessage("a"); err != nil {
		t.Fatalf("DeleteMessage() = %v", err)
	}
	checkStatus(t, mbox, 2, 0)
}

// Until a mailbox is synchronized its local database is incomplete, so the
// counts reported by the API are the only ones available.
func TestStatusUninitializedUsesAPICounts(t *testing.T) {
	mbox, _ := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 1)})
	mbox.initialized = false
	mbox.setAPICounts(40, 7)
	checkStatus(t, mbox, 40, 7)
}

// FETCH 1:* must reach messages added by events after login.
func TestListMessagesStarReachesNewMessages(t *testing.T) {
	mbox, db := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0)})
	mbox.setAPICounts(1, 0)
	if _, err := db.CreateMessage(inboxMessage("b", 1)); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}

	seqSet, err := imap.ParseSeqSet("1:*")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *imap.Message, 10)
	if err := mbox.ListMessages(false, seqSet, []imap.FetchItem{imap.FetchFlags}, ch); err != nil {
		t.Fatalf("ListMessages() = %v", err)
	}

	var seqNums []uint32
	for msg := range ch {
		seqNums = append(seqNums, msg.SeqNum)
	}
	if len(seqNums) != 2 || seqNums[0] != 1 || seqNums[1] != 2 {
		t.Errorf("ListMessages(1:*) returned sequence numbers %v, want [1 2]", seqNums)
	}
}

// recount counts the messages of mboxDB from its entries, the slow way the
// running counts replace.
func recount(t *testing.T, db *database.User, mboxDB *database.Mailbox) (total, unread int) {
	t.Helper()
	err := mboxDB.ForEach(func(_, _ uint32, apiID string) error {
		total++
		msg, err := db.Message(apiID)
		if err == database.ErrNotFound {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Unread == 1 {
			unread++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach() = %v", err)
	}
	return total, unread
}

func TestCountsMatchRecount(t *testing.T) {
	db := openTestDB(t)
	msg := func(id string, unread int, labelIDs ...string) *protonmail.Message {
		return &protonmail.Message{ID: id, Unread: unread, LabelIDs: labelIDs}
	}
	inbox, archive, allMail := protonmail.LabelInbox, protonmail.LabelArchive, protonmail.LabelAllMail
	mailboxes := map[string]*database.Mailbox{
		inbox:   syncTestMailbox(t, db, inbox, []*protonmail.Message{msg("a", 1, inbox, allMail), msg("b", 0, inbox, allMail)}),
		archive: syncTestMailbox(t, db, archive, []*protonmail.Message{msg("c", 1, archive, allMail)}),
	}
	// Synchronized last, so its entries point at messages already stored.
	mailboxes[allMail] = syncTestMailbox(t, db, allMail, []*protonmail.Message{
		msg("a", 1, inbox, allMail), msg("b", 0, inbox, allMail), msg("c", 1, archive, allMail),
	})

	read, unread := 0, 1
	steps := []struct {
		name string
		do   func() error
	}{
		{"synchronized", func() error { return nil }},
		{"new unread message", func() error {
			_, err := db.CreateMessage(msg("d", 1, inbox, allMail))
			return err
		}},
		{"move to archive", func() error {
			_, _, err := db.UpdateMessage("a", &protonmail.EventMessageUpdate{LabelIDs: []string{archive, allMail}})
			return err
		}},
		{"mark read", func() error {
			_, _, err := db.UpdateMessage("c", &protonmail.EventMessageUpdate{Unread: &read})
			return err
		}},
		{"mark unread and move", func() error {
			_, _, err := db.UpdateMessage("b", &protonmail.EventMessageUpdate{
				Unread: &unread, LabelIDsAdded: []string{archive}, LabelIDsRemoved: []string{inbox},
			})
			return err
		}},
		{"delete", func() error {
			_, err := db.DeleteMessage("d")
			return err
		}},
		{"recreate existing message", func() error {
			_, err := db.CreateMessage(msg("c", 1, archive, allMail))
			return err
		}},
		{"reset messages", db.ResetMessages},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%v: %v", step.name, err)
		}
		for labelID, mboxDB := range mailboxes {
			total, unread, err := mboxDB.Count()
			if err != nil {
				t.Fatalf("%v: Count() of %v = %v", step.name, labelID, err)
			}
			wantTotal, wantUnread := recount(t, db, mboxDB)
			if total != wantTotal || unread != wantUnread {
				t.Errorf("%v: Count() of %v = %v, %v, want %v, %v", step.name, labelID, total, unread, wantTotal, wantUnread)
			}
		}
	}
}

// Proton's events carry no message counts, so after a message event the counts
// are fetched again for mailboxes that aren't synchronized.
func TestEventRefreshesAPICounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages/count" {
			t.Errorf("unexpected request to %v", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Code":1000,"Counts":[{"LabelID":"0","Total":3,"Unread":1}]}`))
	}))
	defer srv.Close()

	mbox, _ := newTestMailbox(t, nil)
	mbox.initialized = false
	mbox.setAPICounts(2, 0)
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}
	u.u = &protonmail.User{Name: "jon"}
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: mbox}
	u.eventSent = make(chan struct{})

	events := make(chan *protonmail.Event, 1)
	events <- &protonmail.Event{Messages: []*protonmail.EventMessage{{
		ID:      "c",
		Action:  protonmail.EventCreate,
		Created: inboxMessage("c", 1),
	}}}
	close(events)
	updates := make(chan imapbackend.Update, 10)
	u.receiveEvents(updates, events)

	checkStatus(t, mbox, 3, 1)

	// Let the goroutine waiting for the updates to be sent finish, as the
	// server would.
	close(updates)
	for update := range updates {
		close(update.Done())
	}
}

// Commands waiting for an event, such as STORE, must not also wait for the
// counts fetched after it.
func TestEventSentBeforeCountsFetched(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Code":1000,"Counts":[{"LabelID":"0","Total":3,"Unread":1}]}`))
	}))
	defer srv.Close()

	mbox, _ := newTestMailbox(t, nil)
	mbox.initialized = false
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}
	u.u = &protonmail.User{Name: "jon"}
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: mbox}
	// Buffered, so the signal isn't dropped while this test isn't waiting.
	u.eventSent = make(chan struct{}, 1)

	events := make(chan *protonmail.Event, 1)
	events <- &protonmail.Event{Messages: []*protonmail.EventMessage{{
		ID:      "c",
		Action:  protonmail.EventCreate,
		Created: inboxMessage("c", 1),
	}}}
	close(events)
	updates := make(chan imapbackend.Update, 10)
	done := make(chan struct{})
	go func() {
		u.receiveEvents(updates, events)
		close(done)
	}()

	// The request for counts is still pending.
	close((<-updates).Done())
	<-u.eventSent

	close(release)
	<-done
	checkStatus(t, mbox, 3, 1)
}
