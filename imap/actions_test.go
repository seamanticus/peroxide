package imap

import (
	"encoding/json"
	"errors"
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
	"github.com/emersion/go-message/mail"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// fakeAPI answers message actions with a result per message: codes holds the
// code of each message, 1000 when missing. gets answers GET requests by path,
// with a status and a body. labelIDs gives the labels of each message for the
// metadata request (POST /mail/v4/messages overridden as GET). It records the
// requests made.
type fakeAPI struct {
	t        *testing.T
	codes    map[string]int
	gets     map[string]fakeResponse
	labelIDs map[string][]string

	mu       sync.Mutex
	requests []string
}

type fakeResponse struct {
	status      int
	contentType string
	body        string
}

func (api *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	api.requests = append(api.requests, r.Method+" "+r.URL.Path)
	api.mu.Unlock()

	if res, ok := api.gets[r.URL.Path]; ok && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", res.contentType)
		w.WriteHeader(res.status)
		io.WriteString(w, res.body)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/mail/v4/messages" {
		api.serveMetadata(w, r)
		return
	}
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

// serveMetadata answers the metadata of the messages asked for by ID, those
// in labelIDs.
func (api *fakeAPI) serveMetadata(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-HTTP-Method-Override"); got != http.MethodGet {
		api.t.Errorf("metadata request overridden as %q, want GET", got)
	}
	var req struct{ ID []string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.t.Errorf("decode metadata request: %v", err)
	}
	var msgs []*protonmail.Message
	for _, id := range req.ID {
		if labelIDs, ok := api.labelIDs[id]; ok {
			msgs = append(msgs, &protonmail.Message{ID: id, LabelIDs: labelIDs})
		}
	}
	b, err := json.Marshal(struct {
		Code     int
		Messages []*protonmail.Message
	}{1000, msgs})
	if err != nil {
		api.t.Errorf("encode metadata response: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
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

// trashMessage is a read message in Trash.
func trashMessage(id string) *protonmail.Message {
	return &protonmail.Message{ID: id, LabelIDs: []string{protonmail.LabelTrash, protonmail.LabelAllMail}}
}

// addTestMailbox gives u a mailbox for labelID, synced with msgs.
func addTestMailbox(t *testing.T, u *user, name, labelID string, msgs []*protonmail.Message) *mailbox {
	t.Helper()
	mboxDB := syncTestMailbox(t, u.db, labelID, msgs)
	mbox := &mailbox{name: name, label: labelID, u: u, db: mboxDB, deleted: make(map[string]struct{}), initialized: true}
	u.mailboxes[labelID] = mbox
	return mbox
}

// EXPUNGE in Trash removes the message from Trash, then deletes it for good
// only if no mailbox or label of the account still holds it.
func TestExpungeFromTrash(t *testing.T) {
	const (
		unlabel  = "PUT /messages/unlabel"
		metadata = "POST /mail/v4/messages"
		labels   = "GET /labels"
		del      = "PUT /messages/delete"
	)
	for _, tc := range []struct {
		name string
		// labelIDs are the message's labels in Proton after the unlabel; nil
		// when Proton doesn't have it.
		labelIDs     []string
		listLabels   string // GET /labels body; empty when not expected
		wantRequests []string
		wantDeleted  bool
	}{
		{
			name:         "only in All Mail",
			labelIDs:     []string{protonmail.LabelAllMail, protonmail.LabelAllSent, protonmail.LabelAllDraft},
			wantRequests: []string{unlabel, metadata, del},
			wantDeleted:  true,
		},
		{
			name:         "still in Trash",
			labelIDs:     []string{protonmail.LabelTrash, protonmail.LabelAllMail},
			wantRequests: []string{unlabel, metadata, del},
			wantDeleted:  true,
		},
		{
			name:         "in Inbox",
			labelIDs:     []string{protonmail.LabelAllMail, protonmail.LabelInbox},
			wantRequests: []string{unlabel, metadata},
		},
		{
			name:         "in a label not known yet",
			labelIDs:     []string{protonmail.LabelAllMail, "x"},
			listLabels:   `{"Code":1000,"Labels":[{"ID":"x","Name":"x","Type":1}]}`,
			wantRequests: []string{unlabel, metadata, labels},
		},
		{
			name:         "in a label not listed",
			labelIDs:     []string{protonmail.LabelAllMail, "x"},
			listLabels:   `{"Code":1000,"Labels":[]}`,
			wantRequests: []string{unlabel, metadata, labels, del},
			wantDeleted:  true,
		},
		{
			name:         "gone from Proton",
			wantRequests: []string{unlabel, metadata},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{t: t, labelIDs: map[string][]string{}}
			if tc.labelIDs != nil {
				api.labelIDs["a"] = tc.labelIDs
			}
			if tc.listLabels != "" {
				api.gets = map[string]fakeResponse{"/labels": {http.StatusOK, "application/json", tc.listLabels}}
			}
			inbox, sent := newActionTestMailbox(t, api, nil)
			trash := addTestMailbox(t, inbox.u, "Trash", protonmail.LabelTrash, []*protonmail.Message{trashMessage("a")})

			if err := trash.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
				t.Fatalf("UpdateMessagesFlags() = %v", err)
			}
			if err := trash.Expunge(); err != nil {
				t.Fatalf("Expunge() = %v", err)
			}
			checkStatus(t, trash, 0, 0)
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			if updates := sent(); len(updates) != 1 {
				t.Errorf("sent %v updates, want 1 EXPUNGE", len(updates))
			} else if up, ok := updates[0].(*imapbackend.ExpungeUpdate); !ok || up.SeqNum != 1 {
				t.Errorf("sent %#v, want EXPUNGE 1", updates[0])
			}

			_, err := inbox.u.db.Message("a")
			if tc.wantDeleted && !errors.Is(err, database.ErrNotFound) {
				t.Errorf("db.Message() = %v, want %v: deleted for good", err, database.ErrNotFound)
			} else if !tc.wantDeleted && err != nil {
				t.Errorf("db.Message() = %v, want the message kept", err)
			}
		})
	}
}

// Every message is in All Mail, so EXPUNGE there could only delete them for
// good: it is refused.
func TestExpungeAllMailRefused(t *testing.T) {
	api := &fakeAPI{t: t}
	inbox, _ := newActionTestMailbox(t, api, nil)
	all := addTestMailbox(t, inbox.u, "All Mail", protonmail.LabelAllMail, []*protonmail.Message{trashMessage("a")})

	if err := all.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}
	if err := all.Expunge(); err == nil {
		t.Error("Expunge() = nil, want an error")
	}
	checkStatus(t, all, 1, 0)
	if got := api.madeRequests(); len(got) != 0 {
		t.Errorf("made requests %q, want none", got)
	}
}

func TestSearchMessages(t *testing.T) {
	mbox, _ := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 0), inboxMessage("c", 1)})
	if err := mbox.UpdateMessagesFlags(true, uidSet(3), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}

	for _, tc := range []struct {
		name     string
		isUID    bool
		criteria *imap.SearchCriteria
		want     []uint32
	}{
		{"all", false, &imap.SearchCriteria{}, []uint32{1, 2, 3}},
		{"unseen", false, &imap.SearchCriteria{WithoutFlags: []string{imap.SeenFlag}}, []uint32{1, 3}},
		{"deleted by UID", true, &imap.SearchCriteria{WithFlags: []string{imap.DeletedFlag}}, []uint32{3}},
		{"unseen, not deleted", false, &imap.SearchCriteria{
			WithoutFlags: []string{imap.SeenFlag},
			Not:          []*imap.SearchCriteria{{WithFlags: []string{imap.DeletedFlag}}},
		}, []uint32{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mbox.SearchMessages(tc.isUID, tc.criteria)
			if err != nil {
				t.Fatalf("SearchMessages() = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("SearchMessages() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A message the API returns without a Sender has no From, rather than
// crashing the connection that fetches or searches it.
// evidence: witnesses
func TestNoSender(t *testing.T) {
	msg := inboxMessage("a", 0)
	if env := fetchEnvelope(msg); env.From != nil {
		t.Errorf("fetchEnvelope().From = %v, want nil", env.From)
	}
	if h := messageHeader(msg); h.Has("From") {
		t.Errorf("messageHeader() has From %q, want none", h.Get("From"))
	}
}

// RFC 3501 section 7.4.2: Sender and Reply-To default to From, and the
// envelope's Message-Id is the header's, brackets included.
// evidence: witnesses
func TestEnvelopeDefaults(t *testing.T) {
	from := &protonmail.MessageAddress{Name: "Bob", Address: "bob@example.org"}
	replyTo := &protonmail.MessageAddress{Address: "list@example.org"}
	tests := []struct {
		name        string
		replyTos    []*protonmail.MessageAddress
		wantReplyTo string
	}{
		{"no Reply-To", nil, "bob@example.org"},
		{"Reply-To", []*protonmail.MessageAddress{replyTo}, "list@example.org"},
	}
	for _, tt := range tests {
		msg := inboxMessage("a", 0)
		msg.Sender = from
		msg.ReplyTos = tt.replyTos
		msg.ExternalID = "abc@example.org"
		env := fetchEnvelope(msg)
		if len(env.Sender) != 1 || env.Sender[0].Address() != "bob@example.org" {
			t.Errorf("%v: Sender = %v, want From", tt.name, env.Sender)
		}
		if len(env.ReplyTo) != 1 || env.ReplyTo[0].Address() != tt.wantReplyTo {
			t.Errorf("%v: ReplyTo = %v, want %v", tt.name, env.ReplyTo, tt.wantReplyTo)
		}
		if h := messageHeader(msg); env.MessageId != h.Get("Message-Id") || env.MessageId != "<abc@example.org>" {
			t.Errorf("%v: envelope Message-Id %q, header %q, want <abc@example.org>", tt.name, env.MessageId, h.Get("Message-Id"))
		}
	}
}

// A full message's header is Proton's original one, with threading fields
// and the sent Date kept, the metadata's addresses and subject over it, and
// peroxide's own Content-Type. One that can't be parsed is ignored.
// evidence: witnesses
func TestMessageHeaderFromOriginal(t *testing.T) {
	const original = "Return-Path: <bob@example.org>\r\n" +
		"Date: Mon, 02 Nov 2026 10:00:00 +1300\r\n" +
		"From: Old Name <bob@example.org>\r\n" +
		"To: bob@protonmail.com\r\n" +
		"Subject: original subject\r\n" +
		"Message-Id: <orig@example.org>\r\n" +
		"In-Reply-To: <parent@example.org>\r\n" +
		"References: <root@example.org> <parent@example.org>\r\n" +
		"Mime-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n"

	msg := fetchTestMessage("a", 0)
	msg.Sender = &protonmail.MessageAddress{Name: "Bob", Address: "bob@example.org"}
	msg.Subject = "decrypted subject"
	msg.Header = original
	h := messageHeader(msg)
	for k, want := range map[string]string{
		"Return-Path": "<bob@example.org>",
		"Date":        "Mon, 02 Nov 2026 10:00:00 +1300",
		"Message-Id":  "<orig@example.org>",
		"In-Reply-To": "<parent@example.org>",
		"References":  "<root@example.org> <parent@example.org>",
		"Subject":     "decrypted subject",
		"To":          "bob@protonmail.com",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%v = %q, want %q", k, got, want)
		}
	}
	if from, err := (&mail.Header{Header: h}).AddressList("From"); err != nil || len(from) != 1 ||
		from[0].Name != "Bob" || from[0].Address != "bob@example.org" {
		t.Errorf("From = %v, %v, want the metadata's Bob <bob@example.org>", from, err)
	}
	if mt, _, err := h.ContentType(); err != nil || mt != "multipart/mixed" {
		t.Errorf("Content-Type = %q, %v, want multipart/mixed", mt, err)
	}
	if h.Has("Content-Transfer-Encoding") {
		t.Errorf("Content-Transfer-Encoding = %q, want none", h.Get("Content-Transfer-Encoding"))
	}

	msg.Header = " starts with a space\r\n"
	if h := messageHeader(msg); h.Get("Message-Id") != "<"+messageID(msg)+">" || h.Get("Subject") != "decrypted subject" {
		t.Errorf("with a malformed header: Message-Id %q, Subject %q, want the metadata's", h.Get("Message-Id"), h.Get("Subject"))
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
