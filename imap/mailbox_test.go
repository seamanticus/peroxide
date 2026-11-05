package imap

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

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
		u:           &user{db: db, u: &protonmail.User{Name: "bob"}, messages: newMessageCache(), syncSem: make(chan struct{}, 1)},
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
	u.u = &protonmail.User{Name: "bob"}
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
	u.u = &protonmail.User{Name: "bob"}
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

func mailboxNames(u *user) map[string]string {
	names := make(map[string]string)
	for labelID, mbox := range u.mailboxes {
		names[labelID] = mbox.Name()
	}
	return names
}

// Folders and labels created, renamed and deleted in Proton change mailboxes
// and flags without waiting for the next login, and without telling clients.
func TestLabelEvents(t *testing.T) {
	inbox, _ := newTestMailbox(t, nil)
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	u.flags = map[string]string{protonmail.LabelStarred: imap.FlaggedFlag}

	folder := func(name string) *protonmail.Label {
		return &protonmail.Label{ID: "f", Name: name, Type: protonmail.LabelMessage, Exclusive: 1}
	}
	label := func(name string) *protonmail.Label {
		return &protonmail.Label{ID: "l", Name: name, Type: protonmail.LabelMessage}
	}
	inboxOnly := map[string]string{protonmail.LabelInbox: imap.InboxName}
	withFolder := map[string]string{protonmail.LabelInbox: imap.InboxName, "f": "Work"}
	renamed := map[string]string{protonmail.LabelInbox: imap.InboxName, "f": "Projects"}
	starredOnly := map[string]string{protonmail.LabelStarred: imap.FlaggedFlag}
	withLabel := map[string]string{protonmail.LabelStarred: imap.FlaggedFlag, "l": "To_do"}

	steps := []struct {
		name          string
		event         *protonmail.EventLabel
		wantMailboxes map[string]string
		wantFlags     map[string]string
	}{
		{"create folder", &protonmail.EventLabel{ID: "f", Action: protonmail.EventCreate, Label: folder("Work")}, withFolder, starredOnly},
		{"create label", &protonmail.EventLabel{ID: "l", Action: protonmail.EventCreate, Label: label("To do")}, withFolder, withLabel},
		{"create contact group", &protonmail.EventLabel{ID: "c", Action: protonmail.EventCreate, Label: &protonmail.Label{ID: "c", Name: "Friends", Type: protonmail.LabelContact}}, withFolder, withLabel},
		// Sent after every read or unread change in the folder.
		{"update without changes", &protonmail.EventLabel{ID: "f", Action: protonmail.EventUpdate, Label: folder("Work")}, withFolder, withLabel},
		// Seen in Proton's events: names and Exclusive don't match INBOX or
		// \Flagged, and the event ID differs from the label ID.
		{"update system folder", &protonmail.EventLabel{ID: "k7rAD-", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: protonmail.LabelInbox, Name: "Inbox", Type: protonmail.LabelMessage}}, withFolder, withLabel},
		{"update system label", &protonmail.EventLabel{ID: "ZIZtzh", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: protonmail.LabelStarred, Name: "Starred", Type: protonmail.LabelMessage}}, withFolder, withLabel},
		{"update unknown label", &protonmail.EventLabel{ID: "u", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: "u", Name: "Primary", Type: protonmail.LabelMessage}}, withFolder, withLabel},
		{"rename folder", &protonmail.EventLabel{ID: "f", Action: protonmail.EventUpdate, Label: folder("Projects")}, renamed, withLabel},
		{"rename label", &protonmail.EventLabel{ID: "l", Action: protonmail.EventUpdateFlags, Label: label("Later on")}, renamed, map[string]string{protonmail.LabelStarred: imap.FlaggedFlag, "l": "Later_on"}},
		{"folder becomes label", &protonmail.EventLabel{ID: "f", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: "f", Name: "Other", Type: protonmail.LabelMessage}}, renamed, map[string]string{protonmail.LabelStarred: imap.FlaggedFlag, "l": "Later_on"}},
		{"delete label", &protonmail.EventLabel{ID: "l", Action: protonmail.EventDelete}, renamed, starredOnly},
		{"delete folder", &protonmail.EventLabel{ID: "f", Action: protonmail.EventDelete}, inboxOnly, starredOnly},
	}

	var created *mailbox
	for _, step := range steps {
		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{Labels: []*protonmail.EventLabel{step.event}}
		close(events)
		updates := make(chan imapbackend.Update, 10)
		u.receiveEvents(updates, events)

		if n := len(updates); n != 0 {
			t.Errorf("%v: sent %v updates, want none", step.name, n)
		}
		if got := mailboxNames(u); !maps.Equal(got, step.wantMailboxes) {
			t.Errorf("%v: mailboxes = %v, want %v", step.name, got, step.wantMailboxes)
		}
		if !maps.Equal(u.flags, step.wantFlags) {
			t.Errorf("%v: flags = %v, want %v", step.name, u.flags, step.wantFlags)
		}

		if mbox, ok := u.mailboxes["f"]; ok {
			if created == nil {
				created = mbox
			} else if mbox != created {
				// A new mailbox would lose the old one's UIDs and selections.
				t.Errorf("%v: folder's mailbox replaced", step.name)
			}
		}
	}

	if created == nil {
		t.Fatal("folder never created")
	}
	if _, err := created.db.UidNext(); err == nil {
		t.Error("deleted folder's mailbox still in the local database")
	}
}

func addressEmails(addrs []*protonmail.Address) []string {
	var emails []string
	for _, a := range addrs {
		emails = append(emails, a.Email)
	}
	return emails
}

// Addresses created, changed and deleted in Proton change the address list
// drafts take their sender from, without changing a list handed out before.
func TestAddressEvents(t *testing.T) {
	inbox, _ := newTestMailbox(t, nil)
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	u.addrs = []*protonmail.Address{{ID: "a1", Email: "bob@proton.me"}}

	steps := []struct {
		name  string
		event *protonmail.EventAddress
		want  []string
	}{
		{"create", &protonmail.EventAddress{ID: "a2", Action: protonmail.EventCreate, Address: &protonmail.Address{ID: "a2", Email: "alias@proton.me"}}, []string{"bob@proton.me", "alias@proton.me"}},
		{"update", &protonmail.EventAddress{ID: "a2", Action: protonmail.EventUpdate, Address: &protonmail.Address{ID: "a2", Email: "alias2@proton.me"}}, []string{"bob@proton.me", "alias2@proton.me"}},
		{"update unknown creates", &protonmail.EventAddress{ID: "a3", Action: protonmail.EventUpdateFlags, Address: &protonmail.Address{ID: "a3", Email: "new@proton.me"}}, []string{"bob@proton.me", "alias2@proton.me", "new@proton.me"}},
		{"update without address", &protonmail.EventAddress{ID: "a1", Action: protonmail.EventUpdate}, []string{"bob@proton.me", "alias2@proton.me", "new@proton.me"}},
		{"delete", &protonmail.EventAddress{ID: "a2", Action: protonmail.EventDelete}, []string{"bob@proton.me", "new@proton.me"}},
		{"delete unknown", &protonmail.EventAddress{ID: "zz", Action: protonmail.EventDelete}, []string{"bob@proton.me", "new@proton.me"}},
		{"unknown action", &protonmail.EventAddress{ID: "a1", Action: 9}, []string{"bob@proton.me", "new@proton.me"}},
	}
	for _, step := range steps {
		before := u.addresses()
		kept := slices.Clone(addressEmails(before))

		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{Addresses: []*protonmail.EventAddress{step.event}}
		close(events)
		u.receiveEvents(make(chan imapbackend.Update, 10), events)

		if got := addressEmails(u.addresses()); !slices.Equal(got, step.want) {
			t.Errorf("%v: addresses = %v, want %v", step.name, got, step.want)
		}
		if got := addressEmails(before); !slices.Equal(got, kept) {
			t.Errorf("%v: list read before changed to %v, was %v", step.name, got, kept)
		}
	}
}

// roundTripFunc answers HTTP requests in memory: unlike an httptest server,
// it starts no goroutine, so it works inside a synctest bubble.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The last Logout can come while the events goroutine is still applying a
// page, blocked sending its updates: go-imap calls Logout with its server
// lock held, and its update loop needs that lock to take more, so Logout must
// not wait. The page then finishes with the session intact (its counts
// request needs the client), and only then is the database closed and the
// account removed from the backend; a login meanwhile finds the session
// closing (getUser waits for it).
func TestLogoutWhileApplyingEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inbox, db := newTestMailbox(t, nil)
		// Unbuffered and unread: the page blocks on its first update, as when
		// go-imap's update loop waits for the server lock Logout runs under.
		be := &backend{users: make(map[string]*user), updates: make(chan imapbackend.Update)}
		u := inbox.u
		u.backend = be
		u.username = "bob"
		u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
		u.flags = map[string]string{}
		u.numClients = 1
		u.done = make(chan struct{})
		var countRequests int // only the events goroutine writes it
		u.c = &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/messages/count" {
					t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
				}
				countRequests++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"Code":1000,"Counts":[{"LabelID":"0","Total":0,"Unread":0}]}`)),
					Request:    r,
				}, nil
			}),
		}}
		be.users["bob"] = u
		syncTestMailbox(t, db, protonmail.LabelInbox, []*protonmail.Message{inboxMessage("a", 0)})

		// A message deleted: its EXPUNGE is the page's one update, and once it
		// is taken the page asks the API for message counts.
		events := make(chan *protonmail.Event)
		u.startEvents(be.updates, events)
		events <- &protonmail.Event{ID: "e1", Messages: []*protonmail.EventMessage{{
			ID:     "a",
			Action: protonmail.EventDelete,
		}}}
		synctest.Wait() // blocked sending the page's EXPUNGE

		if err := u.Logout(); err != nil {
			t.Fatalf("Logout() = %v", err)
		}
		close(events) // as the receiver does once done is closed
		synctest.Wait()

		be.Lock()
		kept := be.users["bob"] == u
		be.Unlock()
		if !kept {
			t.Error("the account left the backend before its events stopped")
		}
		if _, _, err := db.LastEventID(); err != nil {
			t.Errorf("the database closed before its events stopped: %v", err)
		}

		// What go-imap does once Logout has returned: take the update.
		close((<-be.updates).Done())
		u.eventLoop.Wait()

		if countRequests != 1 {
			t.Errorf("made %v counts requests after Logout, want 1: the page finishes with the session", countRequests)
		}

		be.Lock()
		_, still := be.users["bob"]
		be.Unlock()
		if still {
			t.Error("the account is still in the backend after its events stopped")
		}
		if u.u != nil || u.c != nil || u.privateKeys != nil {
			t.Error("the session was kept after its events stopped")
		}
		if _, _, err := db.LastEventID(); err == nil {
			// Reopening below would wait forever on bolt's file lock.
			t.Fatal("the database is still open after its events stopped")
		}

		// The page cut by Logout was recorded before the database closed: the
		// next session resumes after it.
		reopened, err := database.Open("test.db")
		if err != nil {
			t.Fatalf("reopen database: %v", err)
		}
		if id, ok, err := reopened.LastEventID(); err != nil || !ok || id != "e1" {
			t.Errorf("stored last event ID = %q, %v, %v, want e1", id, ok, err)
		}
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened database: %v", err)
		}
	})
}

// An event page that arrives while a mailbox syncs is applied after the sync,
// so a message it adds gets a UID after those the sync listed. On 2026-11-05
// a message received while peroxide was idle got UID 1, below the older ones,
// because its replayed create event landed in the mailbox the sync had just
// emptied, and aerc showed it last.
func TestEventDuringSyncKeepsOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inbox, db := newTestMailbox(t, nil)
		inbox.initialized = false
		u := inbox.u
		u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
		u.flags = map[string]string{}

		// The sync's listing waits on release, as a slow Proton would.
		listing, release := make(chan struct{}), make(chan struct{})
		u.c = &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"Code":1000,"Counts":[{"LabelID":"0","Total":3,"Unread":0}]}`
				if r.URL.Path == "/messages" {
					close(listing)
					<-release
					body = `{"Code":1000,"Total":2,"Messages":[` +
						`{"ID":"old1","LabelIDs":["0"]},{"ID":"old2","LabelIDs":["0"]}]}`
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			}),
		}}

		synced := make(chan error, 1)
		go func() { synced <- inbox.init() }()
		<-listing // the sync has emptied INBOX and waits for its listing

		updates := make(chan imapbackend.Update, 10)
		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{ID: "e1", Messages: []*protonmail.EventMessage{{
			ID:      "new",
			Action:  protonmail.EventCreate,
			Created: inboxMessage("new", 1),
		}}}
		close(events)
		applied := make(chan struct{})
		go func() {
			u.receiveEvents(updates, events)
			close(applied)
		}()
		synctest.Wait()
		if _, _, err := inbox.db.FromApiID("new"); err == nil {
			t.Error("the event page was applied while the mailbox synced")
		}

		close(release)
		if err := <-synced; err != nil {
			t.Fatalf("init() = %v", err)
		}
		// What go-imap does: deliver the page's updates, so it finishes.
		go func() {
			for update := range updates {
				close(update.Done())
			}
		}()
		<-applied
		close(updates)

		for _, want := range []struct {
			id  string
			uid uint32
		}{{"old1", 1}, {"old2", 2}, {"new", 3}} {
			_, uid, err := inbox.db.FromApiID(want.id)
			if err != nil || uid != want.uid {
				t.Errorf("message %v has UID %v (%v), want %v", want.id, uid, err, want.uid)
			}
		}
		if id, ok, err := db.LastEventID(); err != nil || !ok || id != "e1" {
			t.Errorf("stored last event ID = %q, %v, %v, want e1", id, ok, err)
		}
	})
}
