package imap

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/responses"
	imapserver "github.com/emersion/go-imap/server"

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

// An update made before its folder is renamed in Proton names the mailbox as
// it is when go-imap delivers it, which is when go-imap matches it to the
// clients that have it selected: the EXISTS isn't lost to the rename.
func TestUpdateFollowsRename(t *testing.T) {
	mbox, _ := newTestMailbox(t, nil)
	mbox.setName("Work")
	up := newMailboxUpdate("bob", mbox)
	mbox.setName("Projects")
	if got := up.Mailbox(); got != "Projects" {
		t.Errorf("Mailbox() = %q after the rename, want Projects", got)
	}
	if got := up.Username(); got != "bob" {
		t.Errorf("Username() = %q, want bob", got)
	}
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

// fetchTestMessage is an INBOX message with a plain body, whose header needs
// no API request.
func fetchTestMessage(id string, unread int) *protonmail.Message {
	msg := inboxMessage(id, unread)
	msg.Sender = &protonmail.MessageAddress{Address: "from@example.org"}
	msg.MIMEType = "text/plain"
	msg.Body = "Hello\r\n"
	return msg
}

// cacheFull puts msgs in mbox's full-message cache, as if fetched already, so
// fetching their bodies makes no API request.
func cacheFull(mbox *mailbox, msgs ...*protonmail.Message) {
	for _, msg := range msgs {
		mbox.u.messages.put(msg)
	}
}

// fetchConn is the part of a server connection FETCH uses: its context and
// WriteResp, which drains the FETCH responses into msgs. The embedded nil
// Conn makes the type a Conn; other methods aren't called.
type fetchConn struct {
	imapserver.Conn
	ctx  *imapserver.Context
	msgs []*imap.Message
}

func (c *fetchConn) Context() *imapserver.Context { return c.ctx }

func (c *fetchConn) WriteResp(res imap.WriterTo) error {
	fetch, ok := res.(*responses.Fetch)
	if !ok {
		return fmt.Errorf("unexpected response %T", res)
	}
	for msg := range fetch.Messages {
		c.msgs = append(c.msgs, msg)
	}
	return nil
}

// fetchWith runs FETCH 1:* items through Extension's FETCH, as go-imap's
// server does, on mbox opened read-only or not, and returns what it sent.
func fetchWith(t *testing.T, mbox *mailbox, readOnly bool, items ...imap.FetchItem) ([]*imap.Message, error) {
	t.Helper()
	seqSet, err := imap.ParseSeqSet("1:*")
	if err != nil {
		t.Fatal(err)
	}
	cmd, ok := Extension{}.Command("FETCH")().(*fetchCommand)
	if !ok {
		t.Fatal("Extension doesn't handle FETCH")
	}
	cmd.SeqSet, cmd.Items = seqSet, items
	conn := &fetchConn{ctx: &imapserver.Context{Mailbox: mbox, MailboxReadOnly: readOnly}}
	err = cmd.Handle(conn)
	return conn.msgs, err
}

// fetchAll runs FETCH 1:* with items on mbox opened read-write.
func fetchAll(t *testing.T, mbox *mailbox, items ...imap.FetchItem) ([]*imap.Message, error) {
	t.Helper()
	return fetchWith(t, mbox, false, items...)
}

// In a mailbox opened read-only (EXAMINE), and through go-imap's own FETCH
// (ListMessages), which can't tell, fetching a body leaves it unread
// (RFC 3501 section 6.3.2).
func TestFetchBodyReadOnlyLeavesUnread(t *testing.T) {
	api := &fakeAPI{t: t}
	msg := fetchTestMessage("a", 1)
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	cacheFull(mbox, msg)

	if _, err := fetchWith(t, mbox, true, "BODY[HEADER]"); err != nil {
		t.Fatalf("FETCH in a read-only mailbox: %v", err)
	}

	seqSet, _ := imap.ParseSeqSet("1:*")
	ch := make(chan *imap.Message, 10)
	if err := mbox.ListMessages(false, seqSet, []imap.FetchItem{"BODY[HEADER]"}, ch); err != nil {
		t.Fatalf("ListMessages() = %v", err)
	}

	if got := api.madeRequests(); len(got) != 0 {
		t.Errorf("made requests %v, want none", got)
	}
	if updates := sent(); len(updates) != 0 {
		t.Errorf("sent %v updates, want none", len(updates))
	}
	checkStatus(t, mbox, 1, 1)
}

// UID FETCH answers with the UID even when the client didn't ask for it
// (RFC 3501 section 6.4.8).
func TestUidFetchAddsUid(t *testing.T) {
	api := &fakeAPI{t: t}
	msg := fetchTestMessage("a", 0)
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})

	seqSet, _ := imap.ParseSeqSet("1:*")
	cmd := &fetchCommand{}
	cmd.SeqSet, cmd.Items = seqSet, []imap.FetchItem{imap.FetchFlags}
	conn := &fetchConn{ctx: &imapserver.Context{Mailbox: mbox}}
	if err := cmd.UidHandle(conn); err != nil {
		t.Fatalf("UID FETCH: %v", err)
	}
	if len(conn.msgs) != 1 || conn.msgs[0].Uid != 1 {
		t.Errorf("sent %+v, want message 1 with UID 1", conn.msgs)
	}
}

// Fetching a body section without .PEEK sets \Seen (RFC 3501 section 6.4.5),
// in Proton too, and tells clients with a FETCH of the flags.
// evidence: witnesses
func TestFetchBodySetsSeen(t *testing.T) {
	tests := []struct {
		name     string
		item     imap.FetchItem
		unread   int
		wantSeen bool
	}{
		{"BODY", "BODY[HEADER]", 1, true},
		{"BODY.PEEK", "BODY.PEEK[HEADER]", 1, false},
		{"RFC822.HEADER", imap.FetchRFC822Header, 1, false},
		{"already read", "BODY[HEADER]", 0, false},
		{"no section", imap.FetchFlags, 1, false},
	}
	for _, tt := range tests {
		api := &fakeAPI{t: t}
		msg := fetchTestMessage("a", tt.unread)
		mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{msg})
		cacheFull(mbox, msg)

		msgs, err := fetchAll(t, mbox, tt.item)
		if err != nil {
			t.Fatalf("%v: ListMessages() = %v", tt.name, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("%v: sent %v messages, want 1", tt.name, len(msgs))
		}

		var wantRequests []string
		wantUnseen := uint32(tt.unread)
		if tt.wantSeen {
			wantRequests = []string{"PUT /messages/read"}
			wantUnseen = 0
		}
		if got := api.madeRequests(); !slices.Equal(got, wantRequests) {
			t.Errorf("%v: made requests %v, want %v", tt.name, got, wantRequests)
		}
		checkStatus(t, mbox, 1, wantUnseen)

		updates := sent()
		if !tt.wantSeen {
			if len(updates) != 0 {
				t.Errorf("%v: sent %v updates, want none", tt.name, len(updates))
			}
			continue
		}
		if len(updates) != 1 {
			t.Fatalf("%v: sent %v updates, want 1", tt.name, len(updates))
		}
		if up, ok := updates[0].(*imapbackend.MessageUpdate); !ok || !slices.Contains(up.Message.Flags, imap.SeenFlag) {
			t.Errorf("%v: sent %#v, want a FETCH with \\Seen", tt.name, updates[0])
		}
	}
}

// An item that is neither a known name nor a body section fails FETCH before
// any message is sent, rather than being left out of the answer.
func TestFetchUnknownItem(t *testing.T) {
	for _, item := range []imap.FetchItem{"X-UNKNOWN", "BODY[", "BINARY[1]"} {
		api := &fakeAPI{t: t}
		mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{fetchTestMessage("a", 1), fetchTestMessage("b", 1)})

		msgs, err := fetchAll(t, mbox, imap.FetchFlags, item)
		if err == nil {
			t.Errorf("FETCH %v: ListMessages() succeeded, want an error", item)
		}
		if len(msgs) != 0 {
			t.Errorf("FETCH %v: sent %v messages, want none", item, len(msgs))
		}
		if got := api.madeRequests(); len(got) != 0 {
			t.Errorf("FETCH %v: made requests %v, want none", item, got)
		}
	}
}

// The macros stand for their items (RFC 3501 section 6.4.5), whoever parsed
// the command.
// evidence: witnesses
func TestFetchMacros(t *testing.T) {
	tests := []struct {
		macro imap.FetchItem
		want  []imap.FetchItem
	}{
		{imap.FetchFast, []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size}},
		{imap.FetchAll, []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope}},
		{imap.FetchFull, []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope, imap.FetchBody}},
	}
	for _, tt := range tests {
		api := &fakeAPI{t: t}
		msg := fetchTestMessage("a", 0)
		mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
		cacheFull(mbox, msg)

		msgs, err := fetchAll(t, mbox, tt.macro)
		if err != nil {
			t.Fatalf("FETCH %v: ListMessages() = %v", tt.macro, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("FETCH %v: sent %v messages, want 1", tt.macro, len(msgs))
		}
		got := slices.Sorted(maps.Keys(msgs[0].Items))
		want := slices.Sorted(slices.Values(tt.want))
		if !slices.Equal(got, want) {
			t.Errorf("FETCH %v: sent items %v, want %v", tt.macro, got, want)
		}
	}
}
