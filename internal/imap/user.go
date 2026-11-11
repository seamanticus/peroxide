package imap

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/internal/auth"
	"github.com/seamanticus/peroxide/internal/events"
	"github.com/seamanticus/peroxide/internal/imap/database"
	"github.com/seamanticus/peroxide/internal/msgbuild"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// systemMailboxes in the order LIST gives them, Proton's sidebar order with
// All Mail last.
var systemMailboxes = []struct {
	name  string
	label string
	attrs []string
}{
	{imap.InboxName, protonmail.LabelInbox, nil},
	{"Drafts", protonmail.LabelDraft, []string{imap.DraftsAttr}},
	{"Sent", protonmail.LabelSent, []string{imap.SentAttr}},
	{"Starred", protonmail.LabelStarred, []string{imap.FlaggedAttr}},
	{"Archive", protonmail.LabelArchive, []string{imap.ArchiveAttr}},
	{"Spam", protonmail.LabelSpam, []string{imap.JunkAttr}},
	{"Trash", protonmail.LabelTrash, []string{imap.TrashAttr}},
	{"All Mail", protonmail.LabelAllMail, []string{imap.AllAttr}},
}

var systemFlags = []struct {
	name  string
	label string
}{
	{imap.FlaggedFlag, protonmail.LabelStarred},
	{imap.DraftFlag, protonmail.LabelDraft},
}

// systemFlag returns the flag that shows labelID, if it is one of
// systemFlags.
func systemFlag(labelID string) (string, bool) {
	for _, data := range systemFlags {
		if data.label == labelID {
			return data.name, true
		}
	}
	return "", false
}

type user struct {
	username    string
	backend     *backend
	c           *protonmail.Client
	u           *protonmail.User
	privateKeys openpgp.EntityList

	db             *database.User
	eventsReceiver eventsReceiver
	builds         builds              // messages being built and stored
	mem            memBudget           // memory the builds take
	senders        msgbuild.SenderKeys // public keys of the senders of signed mail
	pacerOnce      sync.Once
	pacerLimiter   *downloadLimiter // requests' pace; use pacer()

	done      chan struct{} // closed by the last Logout
	eventSent chan struct{}
	// The goroutine applying events, started by startEvents. getUser waits
	// for it after a logout.
	eventLoop sync.WaitGroup
	// The download of the account's messages (startDownload): its goroutine,
	// and the channel that stops it, which only newUser and the events
	// goroutine use.
	downloads    sync.WaitGroup
	downloadStop chan struct{}
	// syncSem serializes what changes the stored messages and mailboxes: the
	// storing of a download page, an event page, an action applied without
	// waiting for its event (lockSync, unlockSync). No request is made while
	// holding it, but MessagesByID for a message an update finds missing. A
	// 1-slot channel rather than a sync.Mutex, so that a goroutine waiting on
	// it is durably blocked for testing/synctest. Taken before u's and the
	// mailboxes' locks, and never inside a database transaction.
	syncSem chan struct{}
	// sendSem orders what clients are told: a writer takes it while it still
	// holds syncSem and keeps it while its updates are delivered
	// (unlockSyncAndSend), so updates reach clients in the order the changes
	// were made. A 1-slot channel, as syncSem.
	sendSem chan struct{}
	// Set, under syncSem, when the events were rewound (download done, or a
	// page to retry): pages delivered before the rewind took effect are
	// dropped until the one asked after replayFrom comes (Event.After).
	replayPending bool
	replayFrom    string

	sync.Mutex // protects everything below

	numClients int
	// Set by rebuild and relogin: no login joins this user, which is ending.
	ending    bool
	mailboxes map[string]*mailbox // indexed by label ID
	// Every mailbox by name, and folders and labels in LIST order: rebuilt
	// by nameMailboxes whenever a name may have changed.
	byName map[string]*mailbox
	listed []*mailbox
	// Replaced, never changed in place, by Addresses events: read it with
	// addresses.
	addrs []*protonmail.Address
	// The mailbox a client selected last (prioritize), whose messages the
	// body download stores first; priorityGen counts the selections.
	priorityLabel string
	priorityGen   uint64
}

func getUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList, storeKey auth.StoreKey) (*user, error) {
	for {
		// TODO: logging a user in may take some time, find a way not to lock all
		// other logins during this time
		be.Lock()
		u, ok := be.users[username]
		if !ok {
			u, err := newUser(be, username, c, privateKeys, storeKey)
			if err != nil {
				be.Unlock()
				return nil, err
			}
			be.users[username] = u
			be.Unlock()
			return u, nil
		}

		u.Lock()
		// A user being rebuilt or logged in again is closing too: its
		// clients are being disconnected, and the login that follows starts
		// a user anew, on a wiped database or with the keys unlocked again.
		closing := u.numClients == 0 || u.ending
		if !closing {
			u.numClients++
		}
		u.Unlock()
		be.Unlock()
		if !closing {
			return u, nil
		}

		// Logged out, but still applying its last events, and holding the
		// account's database (closeAfterLogout). Wait without be's lock, so
		// that other accounts still log in and log out meanwhile.
		u.eventLoop.Wait()
	}
}

func newUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList, storeKey auth.StoreKey) (*user, error) {
	u, err := c.GetCurrentUser()
	if err != nil {
		return nil, err
	}

	addrs, err := c.ListAddresses()
	if err != nil {
		return nil, err
	}

	uu := &user{
		username:    username,
		backend:     be,
		c:           c,
		u:           u,
		privateKeys: privateKeys,
		addrs:       addrs,
		eventSent:   make(chan struct{}),
		syncSem:     make(chan struct{}, 1),
		sendSem:     make(chan struct{}, 1),
		numClients:  1,
	}

	db, keyChanged, err := database.Open(u.Name, storeKey)
	if err != nil {
		return nil, err
	}
	if keyChanged {
		log.Printf("The messages of %q were stored under another bridge password: downloading them again", u.Name)
	}
	uu.db = db

	lastEventID, err := uu.initSession()
	if err != nil {
		// bolt locks the file until it is closed: the next login would wait
		// for it forever.
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("cannot close database: %w", closeErr))
		}
		return nil, err
	}

	uu.done = make(chan struct{})
	ch := make(chan *protonmail.Event)
	// The receiver blocks on ch until the events goroutine reads it. Both
	// fields are set before the goroutines that read them start: the
	// download rewinds the receiver, the events goroutine owns downloadStop.
	uu.eventsReceiver = be.eventsManager.Register(c, u.Name, lastEventID, ch, uu.done)
	uu.startDownload()
	uu.startEvents(be.updates, ch)

	log.Printf("User %q logged in via IMAP", u.Name)
	return uu, nil
}

// initSession loads the mailboxes and returns the event ID to resume from:
// the last event applied, even in a previous session, so that the events of
// the time peroxide was stopped are not lost. With none stored, the latest
// event ID is asked for before the labels and messages are listed: a change
// made meanwhile then comes as an event, applied again if the listing
// already had it, rather than being missed (proton-bridge's
// StartSyncEventID). It is stored only once the mailboxes are loaded, so a
// failed login asks again. A database marked for a rebuild by the last
// session is wiped first, as proton-bridge checks ShouldResync at login.
func (u *user) initSession() (lastEventID string, err error) {
	rebuild, err := u.db.NeedsRebuild()
	if err != nil {
		return "", fmt.Errorf("cannot read whether the database needs a rebuild: %w", err)
	}
	if rebuild {
		log.Printf("The database of %q was marked for a rebuild: wiping it", u.u.Name)
		if err := u.db.Wipe(); err != nil {
			return "", fmt.Errorf("cannot wipe the database: %w", err)
		}
	}

	lastEventID, stored, err := u.db.LastEventID()
	if err != nil {
		return "", fmt.Errorf("cannot read last event ID: %w", err)
	}
	if !stored {
		if lastEventID, err = u.c.GetLatestEventID(); err != nil {
			return "", fmt.Errorf("cannot get latest event ID: %w", err)
		}
	}

	labels, err := u.c.ListLabels()
	if err != nil {
		return "", err
	}
	if err := u.setMailboxes(labels); err != nil {
		return "", err
	}

	if !stored {
		if err := u.db.SetLastEventID(lastEventID); err != nil {
			return "", fmt.Errorf("cannot store last event ID: %w", err)
		}
	}
	return lastEventID, nil
}

// labelNameToMailbox makes a Proton label name one level of an IMAP name. A
// "/" would read as nesting, so it becomes U+2215 DIVISION SLASH, which looks
// the same; mailboxToLabelName undoes it. Proton accepts both "/" and "\" in
// names; "\" needs nothing.
func labelNameToMailbox(name string) string {
	return strings.ReplaceAll(name, delimiter, "\u2215")
}

// mailboxToLabelName is the Proton name of one level of an IMAP name given to
// CREATE or RENAME. A name holding U+2215 itself comes back with "/".
func mailboxToLabelName(level string) string {
	return strings.ReplaceAll(level, "\u2215", delimiter)
}

// setMailboxes makes the mailboxes of the system labels, folders and labels,
// creating their buckets if the database has none.
func (u *user) setMailboxes(labels []*protonmail.Label) error {
	u.Lock()
	defer u.Unlock()

	mailboxes := make(map[string]*mailbox)
	for _, data := range systemMailboxes {
		var err error
		mailboxes[data.label], err = newMailbox(data.name, data.label, data.attrs, u)
		if err != nil {
			return err
		}
	}

	for _, label := range labels {
		if !label.HoldsMessages() {
			continue
		}
		if _, ok := mailboxes[label.ID]; ok {
			continue
		}
		mbox, err := newMailbox("", label.ID, nil, u)
		if err != nil {
			return err
		}
		mbox.setLabel(label)
		mailboxes[label.ID] = mbox
	}

	u.mailboxes = mailboxes
	u.nameMailboxes()
	return nil
}

// setLabel takes the kind, name and parent of label. Under u's lock, and
// followed by nameMailboxes.
func (mbox *mailbox) setLabel(label *protonmail.Label) {
	mbox.kind = labelKind(label)
	mbox.labelName = label.Name
	mbox.color = label.Color
	mbox.parentID = ""
	if mbox.kind == kindFolder {
		// Labels can't be nested in Proton.
		mbox.parentID = label.ParentID
	}
}

// nameMailboxes names every folder and label after its kind and, for a
// folder, its parents' names, as proton-bridge does (Folders/<path>,
// Labels/<name>), and rebuilds byName and listed. Under u's lock.
func (u *user) nameMailboxes() {
	names := make(map[string]string, len(u.mailboxes))
	var name func(mbox *mailbox, depth int) string
	name = func(mbox *mailbox, depth int) string {
		if n, ok := names[mbox.label]; ok {
			return n
		}
		level := labelNameToMailbox(mbox.labelName)
		parent, ok := u.mailboxes[mbox.parentID]
		var n string
		switch {
		case mbox.parentID == "":
			n = mbox.kind.prefix() + delimiter + level
		case !ok || parent.kind != kindFolder || depth > len(u.mailboxes):
			// Proton lists every folder with its parents, and a cycle can't
			// be made: neither should happen.
			log.Printf("Folder %v has no parent folder %v: shown at the top", mbox.label, mbox.parentID)
			n = mbox.kind.prefix() + delimiter + level
		default:
			n = name(parent, depth+1) + delimiter + level
		}
		names[mbox.label] = n
		return n
	}

	u.byName = make(map[string]*mailbox, len(u.mailboxes))
	u.listed = u.listed[:0]
	for _, labelID := range slices.Sorted(maps.Keys(u.mailboxes)) {
		mbox := u.mailboxes[labelID]
		if mbox.kind != kindSystem {
			mbox.setName(name(mbox, 0))
			u.listed = append(u.listed, mbox)
		}
		u.byName[mbox.Name()] = mbox
	}
	// Folders before labels, each by name; label IDs (sorted above, and
	// SortStableFunc) break no ties, as names are unique within a kind.
	slices.SortStableFunc(u.listed, func(a, b *mailbox) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.Name(), b.Name()))
	})
}

// hasChildFolder reports whether a folder is in the folder labelID. Under u's
// lock.
func (u *user) hasChildFolder(labelID string) bool {
	for _, mbox := range u.mailboxes {
		if mbox.kind == kindFolder && mbox.parentID == labelID {
			return true
		}
	}
	return false
}

func (u *user) Username() string {
	return u.u.Name
}

// ListMailboxes lists the system mailboxes in systemMailboxes' order, then
// Folders and the folders, then Labels and the labels, each by name: the same
// on every LIST, whatever the order the labels came in. For LSUB (subscribed)
// it leaves out the mailboxes unsubscribed from, and Folders or Labels when
// nothing inside is left.
func (u *user) ListMailboxes(subscribed bool) ([]imapbackend.Mailbox, error) {
	unsubscribed := map[string]bool{}
	if subscribed {
		// Read before u's lock, which commands shouldn't wait on a read for.
		var err error
		if unsubscribed, err = u.db.Unsubscribed(); err != nil {
			return nil, fmt.Errorf("cannot read the subscriptions: %w", err)
		}
	}

	u.Lock()
	defer u.Unlock()

	list := make([]imapbackend.Mailbox, 0, len(u.mailboxes)+2)
	for _, data := range systemMailboxes {
		// Hidden from LIST only, as proton-bridge hides it (gluon's
		// GetMailboxVisibility): SELECT and STATUS still find it by name.
		if data.label == protonmail.LabelAllMail && !u.backend.showAllMail {
			continue
		}
		if mbox, ok := u.mailboxes[data.label]; ok && !unsubscribed[data.label] {
			list = append(list, mbox)
		}
	}
	for _, kind := range []mailboxKind{kindFolder, kindLabel} {
		var children []imapbackend.Mailbox
		hasKind := false
		for _, mbox := range u.listed {
			if mbox.kind != kind {
				continue
			}
			hasKind = true
			if !unsubscribed[mbox.label] {
				children = append(children, mbox)
			}
		}
		if subscribed && len(children) == 0 {
			continue
		}
		list = append(list, placeholder{kind: kind, children: hasKind})
		list = append(list, children...)
	}
	return list, nil
}

func (u *user) getMailboxByLabel(labelID string) *mailbox {
	u.Lock()
	defer u.Unlock()
	return u.mailboxes[labelID]
}

func (u *user) getMailbox(name string) *mailbox {
	u.Lock()
	defer u.Unlock()
	return u.byName[name]
}

func (u *user) GetMailbox(name string) (imapbackend.Mailbox, error) {
	mbox := u.getMailbox(name)
	if mbox == nil {
		return nil, imapbackend.ErrNoSuchMailbox
	}
	return mbox, nil
}

// parseMailboxName splits a name given to CREATE or RENAME into the kind of
// mailbox it names and its levels below Folders or Labels, as IMAP levels
// (mailboxToLabelName gives Proton's names), as proton-bridge's CreateMailbox
// accepts them: Folders/<path>, Labels/<name>. A trailing delimiter is
// dropped (RFC 3501 section 6.3.3).
func parseMailboxName(name string) (mailboxKind, []string, error) {
	levels := strings.Split(strings.TrimSuffix(name, delimiter), delimiter)
	var kind mailboxKind
	switch levels[0] {
	case kindFolder.prefix():
		kind = kindFolder
	case kindLabel.prefix():
		kind = kindLabel
	default:
		return kindSystem, nil, fmt.Errorf("%q: it is not in Folders or Labels: name a folder Folders/<name>, a label Labels/<name>", name)
	}
	levels = levels[1:]
	if len(levels) == 0 {
		return kindSystem, nil, fmt.Errorf("%q: it names no folder or label inside it", name)
	}
	if slices.Contains(levels, "") {
		return kindSystem, nil, fmt.Errorf("%q: it has an empty level", name)
	}
	if kind == kindLabel && len(levels) > 1 {
		return kindSystem, nil, fmt.Errorf("%q: labels can't be nested in Proton", name)
	}
	return kind, levels, nil
}

// isPlaceholder reports whether name is Folders or Labels.
func isPlaceholder(name string) bool {
	return name == kindFolder.prefix() || name == kindLabel.prefix()
}

// CreateMailbox creates a folder or label in Proton, as proton-bridge does
// (connector.go, createFolder and createLabel), and adds its mailbox at once
// from Proton's answer: aerc opens a mailbox it created right away, and the
// Labels event that follows finds it there (createLabel keeps it). The
// folders a new folder is in are created first if missing, as gluon creates
// a mailbox's superiors. CREATE of a mailbox there is, which aerc sends
// before every APPEND, makes no request.
func (u *user) CreateMailbox(name string) error {
	if u.getMailbox(name) != nil || isPlaceholder(name) {
		return imapbackend.ErrMailboxAlreadyExists
	}
	kind, levels, err := parseMailboxName(name)
	if err != nil {
		return fmt.Errorf("cannot create %w", err)
	}
	if kind == kindFolder {
		_, err := u.ensureFolder(levels)
		return err
	}
	label, err := u.c.CreateLabel(mailboxToLabelName(levels[0]), protonmail.LabelMessage, "")
	if err != nil {
		return fmt.Errorf("cannot create label %v: %w", name, err)
	}
	u.createLabel(label)
	return nil
}

// ensureFolder returns the label ID of the folder Folders/<levels...>,
// creating it and the folders it is in where missing. levels empty is the
// top, "".
func (u *user) ensureFolder(levels []string) (string, error) {
	parentID := ""
	for i := range levels {
		name := kindFolder.prefix() + delimiter + strings.Join(levels[:i+1], delimiter)
		if mbox := u.getMailbox(name); mbox != nil {
			parentID = mbox.label
			continue
		}
		label, err := u.c.CreateLabel(mailboxToLabelName(levels[i]), protonmail.LabelFolder, parentID)
		if err != nil {
			return "", fmt.Errorf("cannot create folder %v: %w", name, err)
		}
		asFolder(label, parentID)
		u.createLabel(label)
		parentID = label.ID
	}
	return parentID, nil
}

// asFolder makes Proton's answer to a request for a folder in parentID say
// so in the form events use: the answer may give Type 3 alone, and no
// ParentID.
func asFolder(label *protonmail.Label, parentID string) {
	label.Exclusive = 1
	if label.ParentID == "" {
		label.ParentID = parentID
	}
}

// labelOf returns what a RENAME or DELETE of mbox needs, under u's lock.
func (u *user) labelOf(mbox *mailbox) (kind mailboxKind, color, parentID string) {
	u.Lock()
	defer u.Unlock()
	return mbox.kind, mbox.color, mbox.parentID
}

// systemMailboxError refuses to change a system mailbox or a placeholder, as
// proton-bridge refuses (ErrOperationNotAllowed); INBOX can't be deleted in
// any server (RFC 3501 section 6.3.4).
func systemMailboxError(verb, name string) error {
	return fmt.Errorf("cannot %v %v: it is one of peroxide's own mailboxes", verb, name)
}

// DeleteMailbox deletes a folder or label in Proton (DELETE
// /core/v4/labels/{id}), as proton-bridge does, and removes its mailbox at
// once, as the Labels event would: clients that have it selected get BYE.
func (u *user) DeleteMailbox(name string) error {
	mbox := u.getMailbox(name)
	if mbox == nil {
		if isPlaceholder(name) {
			return systemMailboxError("delete", name)
		}
		return imapbackend.ErrNoSuchMailbox
	}
	if kind, _, _ := u.labelOf(mbox); kind == kindSystem {
		return systemMailboxError("delete", name)
	}
	if err := u.c.DeleteLabel(mbox.label); err != nil {
		return fmt.Errorf("cannot delete %v: %w", name, err)
	}
	// Its bucket is deleted: not while a page fills it (deleteLabel).
	u.lockSync()
	ups := u.deleteLabel(mbox.label)
	u.unlockSyncAndSend(ups, u.sendUpdates)
	return nil
}

// RenameMailbox renames a folder or label in Proton (PUT
// /core/v4/labels/{id} with its color), as proton-bridge does, and applies the
// answer at once. A folder may move into another, created if missing, or to
// the top of Folders with ParentID 0, as the web client moves it, which
// proton-bridge can't. A folder can't become a label or a label a folder,
// and system mailboxes, INBOX too, can't be renamed (RFC 3501's RENAME of
// INBOX moves its messages out, which Proton has no way to do).
func (u *user) RenameMailbox(existingName, newName string) error {
	mbox := u.getMailbox(existingName)
	if mbox == nil {
		if isPlaceholder(existingName) {
			return systemMailboxError("rename", existingName)
		}
		return imapbackend.ErrNoSuchMailbox
	}
	oldKind, color, _ := u.labelOf(mbox)
	if oldKind == kindSystem {
		return systemMailboxError("rename", existingName)
	}
	if u.getMailbox(newName) != nil || isPlaceholder(newName) {
		return imapbackend.ErrMailboxAlreadyExists
	}
	kind, levels, err := parseMailboxName(newName)
	if err != nil {
		return fmt.Errorf("cannot rename to %w", err)
	}
	if kind != oldKind {
		return fmt.Errorf("cannot rename %v to %v: a folder stays a folder and a label a label", existingName, newName)
	}

	parentID := ""
	if kind == kindFolder {
		if strings.HasPrefix(newName, existingName+delimiter) {
			return fmt.Errorf("cannot move %v into itself", existingName)
		}
		if parentID, err = u.ensureFolder(levels[:len(levels)-1]); err != nil {
			return err
		}
	}
	label, err := u.c.UpdateLabel(mbox.label, mailboxToLabelName(levels[len(levels)-1]), color, kind == kindFolder, parentID)
	if err != nil {
		return fmt.Errorf("cannot rename %v: %w", existingName, err)
	}
	if kind == kindFolder {
		asFolder(label, parentID)
		// Where it was asked to go, whatever the answer says: "" is the
		// top, which the answer may show as no ParentID or the old one.
		label.ParentID = parentID
	}
	u.updateLabel(label)
	return nil
}

func (u *user) Logout() error {
	u.backend.Lock()
	defer u.backend.Unlock()
	u.Lock()
	defer u.Unlock()

	if u.numClients <= 0 {
		panic("invariant: go-imap calls Logout once per Login")
	}
	u.numClients--
	if u.numClients > 0 {
		return nil
	}

	log.Printf("User %q logged out via IMAP", u.u.Name)
	// The events goroutine may be applying a page: it closes the database
	// and leaves be.users once it has stopped (closeAfterLogout). Logout
	// can't wait for it: go-imap calls Logout holding its server lock,
	// which delivering that page's updates needs.
	close(u.done)
	return nil
}

// startEvents applies the events received on ch until it is closed, after
// the last Logout, then ends the session: the events goroutine is the last to
// use the database.
func (u *user) startEvents(updates chan<- imapbackend.Update, ch <-chan *protonmail.Event) {
	u.eventLoop.Go(func() {
		u.receiveEvents(updates, ch)
		u.closeAfterLogout()
	})
}

// closeAfterLogout closes the database and lets go of the session once the
// events have stopped, and only then leaves be.users, so that the next login
// of the account (getUser) opens the database after it is closed.
func (u *user) closeAfterLogout() {
	// Before the database closes: the download stops at its next page, as
	// u.done is closed.
	u.stopDownload()
	if err := u.db.Close(); err != nil {
		log.Printf("cannot close the database of %q: %v", u.username, err)
	}

	u.backend.Lock()
	defer u.backend.Unlock()
	u.Lock()
	defer u.Unlock()

	delete(u.backend.users, u.username)
	maps.DeleteFunc(u.backend.logins, func(_ string, lu *user) bool { return lu == u })
	u.c = nil
	u.u = nil
	u.privateKeys = nil
}

// lockSync waits until no other goroutine adds or renumbers mailboxes' UIDs
// (user.syncSem); unlockSync lets the next one.
func (u *user) lockSync() {
	if u.syncSem == nil {
		panic("invariant: newUser makes user.syncSem")
	}
	u.syncSem <- struct{}{}
}

func (u *user) unlockSync() {
	<-u.syncSem
}

// eventsReceiver is what a user asks of the account's events receiver
// (events.Receiver): polls and rewinds.
type eventsReceiver interface {
	Poll() <-chan error
	Rewind(id string)
}

var _ eventsReceiver = (*events.Receiver)(nil) // proof: compile-time

// unlockSyncAndSend releases syncSem and has deliver hand updates to the
// clients. sendSem is taken before syncSem is released, so that the next
// writer, which can only change things after this one, also tells clients
// after it: an EXISTS or EXPUNGE delivered out of order shows a client a
// wrong count, or makes it drop the wrong message.
func (u *user) unlockSyncAndSend(updates []imapbackend.Update, deliver func([]imapbackend.Update)) {
	if u.sendSem == nil {
		panic("invariant: newUser makes user.sendSem")
	}
	u.sendSem <- struct{}{}
	u.unlockSync()
	deliver(updates)
	<-u.sendSem
}

// deliverTimeout bounds how long sendOn waits for go-imap to write one update.
// go-imap v1 hands an update to each connection in a goroutine of its own,
// and never writes it to a connection that ends meanwhile (the goroutine
// blocks on a channel no one reads), so its Done is never closed; without a
// bound, that would hold sendSem, and every later change of the account,
// forever.
const deliverTimeout = time.Minute

// sendOn hands updates to the IMAP server on ch one at a time, each once the
// one before was written to every client concerned: go-imap writes the
// updates it is given at once in no fixed order.
func (u *user) sendOn(ch chan<- imapbackend.Update, updates []imapbackend.Update) {
	for _, update := range updates {
		// go-imap creates Done's channel lazily without synchronization:
		// create it first.
		done := update.Done()
		ch <- update
		select {
		case <-done:
		case <-time.After(deliverTimeout):
			log.Printf("go-imap didn't deliver an update for %q within %v: going on", u.username, deliverTimeout)
		}
	}
}

// rewindEvents makes the events receiver deliver again the pages after id,
// as proton-bridge's RewindEventID. Called under syncSem, which guards
// replayPending and replayFrom: the pages delivered before the rewind takes
// effect are dropped (receiveEvents), as they come again.
func (u *user) rewindEvents(id string) {
	u.replayPending, u.replayFrom = true, id
	if u.eventsReceiver != nil {
		u.eventsReceiver.Rewind(id)
	}
}

// lockSyncUnless takes syncSem, as lockSync, unless stop or u.done is closed
// first: then it reports false, without it.
func (u *user) lockSyncUnless(stop <-chan struct{}) bool {
	if u.syncSem == nil {
		panic("invariant: newUser makes user.syncSem")
	}
	select {
	case <-stop:
		return false
	case <-u.done:
		return false
	default:
	}
	select {
	case u.syncSem <- struct{}{}:
		return true
	case <-stop:
		return false
	case <-u.done:
		return false
	}
}

// poll asks for the events now, and waits until a page is applied, the poll
// fails or the user logged out. NOOP and APPEND wait on it: a failed poll
// (Proton unreachable, rate limited) used to leave them waiting for the next
// poll that succeeds, past clients' timeouts. They answer OK all the same:
// NOOP can't fail (RFC 3501 section 6.1.2), and APPEND has already created
// its message; the updates come with a later poll.
func (u *user) poll() {
	result := u.eventsReceiver.Poll()
	for {
		select {
		case <-u.eventSent:
			return
		case err := <-result:
			if err != nil {
				log.Printf("Answering without new events: %v", err)
				return
			}
			// Its pages are delivered: wait for the last to be applied.
			result = nil
		case <-u.done:
			return
		}
	}
}

func (u *user) receiveEvents(updates chan<- imapbackend.Update, events <-chan *protonmail.Event) {
	// Set once a rebuild is asked for: the pages that follow, until the
	// clients have gone, are drained unapplied, and their IDs not stored.
	rebuilding := false
	deliver := func(ups []imapbackend.Update) { u.sendOn(updates, ups) }
	for event := range events {
		if rebuilding {
			continue
		}
		var eventUpdates []imapbackend.Update
		var failed, retry int
		var changed []string
		newKey := false

		// Applied whole while no download page is stored, and sent in turn
		// (unlockSyncAndSend).
		u.lockSync()
		if u.replayPending {
			if event.After != u.replayFrom {
				// Asked for before the rewind: it comes again in the replay.
				u.unlockSync()
				continue
			}
			u.replayPending = false
		}
		refresh := event.Refresh&protonmail.EventRefreshMail != 0
		checkNames := false
		if !refresh {
			// First, so that messages moved into a new folder find its mailbox.
			for _, eventLabel := range event.Labels {
				ups, check := u.handleLabelEvent(eventLabel)
				eventUpdates = append(eventUpdates, ups...)
				checkNames = checkNames || check
			}
			for _, eventAddress := range event.Addresses {
				if u.handleAddressEvent(eventAddress) {
					newKey = true
				}
			}

			for _, eventMessage := range event.Messages {
				ups, c, err := u.applyMessageEvent(eventMessage)
				eventUpdates = append(eventUpdates, ups...)
				if c && !slices.Contains(changed, eventMessage.ID) {
					changed = append(changed, eventMessage.ID)
				}
				switch {
				case err == nil:
				case protonmail.Retryable(err):
					log.Printf("cannot handle event for message %s now: %v (retrying at the next poll)", eventMessage.ID, err)
					retry++
				default:
					log.Printf("cannot handle event for message %s: %v", eventMessage.ID, err)
					failed++
				}
			}
		}
		switch {
		case refresh || failed > 0:
			// The event ID isn't stored: the rebuild below starts over.
		case retry > 0:
			// As proton-bridge retries an event failed on the network, a
			// rate limit or a server error: the page comes again at the
			// next poll, and what it did is applied again, which is
			// idempotent. Its ID isn't stored, so a restart retries it too.
			u.rewindEvents(event.After)
		default:
			// Under syncSem, before the next writer: the download, once
			// done, sets it back to where it started (replay), which this
			// must not overwrite. A page stopped halfway is applied again
			// by the next session: creates, deletes and label changes are
			// idempotent.
			if err := u.db.SetLastEventID(event.ID); err != nil {
				log.Printf("cannot store last event ID %v: %v", event.ID, err)
			}
		}
		u.unlockSyncAndSend(eventUpdates, deliver)
		if checkNames {
			// A request: without syncSem, as refreshChanged.
			u.checkFolderNames()
		}
		if !refresh && failed == 0 {
			// After the page, without syncSem (refreshChanged).
			u.refreshChanged(changed, deliver)
		}
		select {
		case u.eventSent <- struct{}{}:
		default:
		}

		if refresh || failed > 0 {
			if refresh {
				log.Printf("Proton asked %q to refresh its mail: rebuilding the database", u.u.Name)
			} else {
				log.Printf("%v message events of %q could not be applied: rebuilding the database", failed, u.u.Name)
			}
			u.rebuild(updates)
			rebuilding = true
		} else if newKey {
			// The pages that follow are still applied: nothing is wiped.
			u.relogin(updates)
		}
	}
}

// applyMessageEvent applies one message event to the database, and returns
// the updates telling clients about it. An error means the database may now
// differ from Proton.
// changed reports that the message's contents may have changed (an edited
// draft): the caller builds it again once the page is applied
// (refreshChanged).
func (u *user) applyMessageEvent(eventMessage *protonmail.EventMessage) (updates []imapbackend.Update, changed bool, err error) {
	if eventMessage.Action == protonmail.EventCreate || eventMessage.Action == protonmail.EventUpdate {
		// Proton has a new version of it: the background download tries a
		// message that failed again.
		if err := u.db.RemoveFailed(eventMessage.ID); err != nil {
			return nil, false, fmt.Errorf("cannot clear message %v from the failed ones: %w", eventMessage.ID, err)
		}
	}
	switch eventMessage.Action {
	case protonmail.EventCreate:
		log.Println("Received create event for message", eventMessage.ID)
		// A message the download listed first is updated, keeping its UIDs.
		change, err := u.db.CreateMessage(eventMessage.Created)
		if err != nil {
			return nil, false, fmt.Errorf("cannot create message in local DB: %w", err)
		}
		return u.downloadUpdates([]database.MessageChange{change}), false, nil
	case protonmail.EventUpdate, protonmail.EventUpdateFlags:
		log.Println("Received update event for message", eventMessage.ID)
		ups, err := u.applyUpdate(eventMessage.ID, eventMessage.Updated)
		if errors.Is(err, database.ErrNotFound) {
			created, err := u.updateUnknownMessage(eventMessage.ID)
			return append(ups, created...), false, err
		} else if err != nil {
			return ups, false, err
		}
		// Its contents may have changed, e.g. a draft edited: built again
		// after the page, so that the new UIDs are in the mailboxes it is
		// in then.
		return ups, eventMessage.Action == protonmail.EventUpdate, nil
	case protonmail.EventDelete:
		log.Println("Received delete event for message", eventMessage.ID)
		ups, err := u.applyDelete(eventMessage.ID)
		return ups, false, err
	default:
		// Not a broken invariant: the action comes from the API. Nothing is
		// known to be lost either.
		log.Printf("cannot handle event for message %s: unknown action %v", eventMessage.ID, eventMessage.Action)
		return nil, false, nil
	}
}

// refreshChanged builds again the messages a page of events changed, each
// once whatever the number of its updates in the page (a burst of draft
// autosaves), in parallel and holding no lock, so that the download and the
// IMAP commands don't wait for these requests; renewals are delivered in
// turn as they come (refreshMessage). It returns once all are done, so that
// the next page sees them applied.
func (u *user) refreshChanged(ids []string, deliver func([]imapbackend.Update)) {
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if err := u.refreshMessage(id, deliver); err != nil {
				log.Printf("cannot build message %v again: %v", id, err)
			}
		})
	}
	wg.Wait()
}

// unknownMessageTimeout bounds the one request made while holding syncSem
// (updateUnknownMessage): past it, the page is retried at the next poll.
const unknownMessageTimeout = 30 * time.Second

// updateUnknownMessage handles an update event for a message the database
// lacks. While the download runs it will list the message, as Proton has it
// then. After, the message should be there: it is fetched and created, as
// proton-bridge creates a message an update finds missing. It returns the
// updates telling clients. A network error is retryable.
func (u *user) updateUnknownMessage(apiID string) ([]imapbackend.Update, error) {
	state, err := u.db.SyncState()
	if err != nil {
		return nil, fmt.Errorf("cannot read where the download stands: %w", err)
	}
	if !state.Done {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), unknownMessageTimeout)
	defer cancel()
	msgs, err := u.c.MessagesByID(ctx, []string{apiID})
	if err != nil {
		return nil, fmt.Errorf("cannot get the message missing from the local DB: %w", err)
	}
	var changes []database.MessageChange
	for _, msg := range msgs {
		change, err := u.db.CreateMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("cannot create the message missing from the local DB: %w", err)
		}
		changes = append(changes, change)
	}
	return u.downloadUpdates(changes), nil
}

// rebuild marks the database for a rebuild, stops the download and
// disconnects every client of the account, with BYE, as proton-bridge removes
// its IMAP user on a Refresh or a bad event. The login that follows wipes the
// database and downloads the account again (initSession); peroxide stopped
// before then does it on its next login. Called by the events goroutine only,
// without syncSem.
func (u *user) rebuild(updates chan<- imapbackend.Update) {
	u.Lock()
	u.ending = true
	u.Unlock()
	if err := u.db.SetNeedsRebuild(); err != nil {
		// The page's event ID isn't stored either: the next session applies
		// it again, and fails again or succeeds.
		log.Printf("cannot mark the database of %q for a rebuild: %v", u.u.Name, err)
	}
	u.stopDownload()
	u.bye(updates, "Proton's mail changed in a way peroxide must download again: please log in again")
}

// relogin disconnects every client of the account, with BYE, because the
// account has a key this session hasn't unlocked (markLockedKeys). Nothing
// that unlocks keys is kept: the login that follows brings the bridge
// password, with which auth.Manager unlocks the keys again, and waits for
// this user to end (getUser), so that it starts one with the new keys, which
// builds again the messages left encrypted (refreshFallbacks). Called by the
// events goroutine only, without syncSem.
func (u *user) relogin(updates chan<- imapbackend.Update) {
	u.Lock()
	ending := u.ending
	u.ending = true
	u.Unlock()
	if ending {
		return
	}
	log.Printf("Disconnecting the clients of %q: logging in again unlocks its new key", u.u.Name)
	u.bye(updates, "Your Proton account has a new key: please log in again to unlock it")
}

// bye tells every client of the account why it is disconnected, then ends
// their connections.
func (u *user) bye(updates chan<- imapbackend.Update, info string) {
	bye := &imapbackend.StatusUpdate{
		Update: imapbackend.NewUpdate(u.u.Name, ""),
		StatusResp: &imap.StatusResp{
			Type: imap.StatusRespBye,
			Info: info,
		},
	}
	// In turn after what clients were told before (sendSem), bounded.
	u.sendSem <- struct{}{}
	u.sendOn(updates, []imapbackend.Update{bye})
	<-u.sendSem
	u.backend.disconnect(u)
}

// isSystemLabel reports whether labelID has a mailbox or flag of its own.
func isSystemLabel(labelID string) bool {
	for _, data := range systemMailboxes {
		if data.label == labelID {
			return true
		}
	}
	for _, data := range systemFlags {
		if data.label == labelID {
			return true
		}
	}
	return false
}

// handleLabelEvent applies a folder or label created, renamed or deleted in
// Proton, as initMailboxes would. Clients see it on their next LIST, but
// those with a deleted mailbox selected get the BYE it returns. checkNames
// asks for checkFolderNames: a folder holding others got a new name.
func (u *user) handleLabelEvent(event *protonmail.EventLabel) (ups []imapbackend.Update, checkNames bool) {
	// Proton updates system labels after every read or unread change, with
	// names and Exclusive values that don't match their mailboxes and flags.
	// Their events have IDs of their own: only Label holds the label ID.
	labelID := event.ID
	if event.Label != nil {
		labelID = event.Label.ID
	}
	if isSystemLabel(labelID) {
		return nil, false
	}

	switch event.Action {
	case protonmail.EventCreate:
		if event.Label == nil {
			log.Printf("cannot handle create event for label %v: no label", event.ID)
			return nil, false
		}
		u.createLabel(event.Label)
	case protonmail.EventUpdate, protonmail.EventUpdateFlags:
		if event.Label == nil {
			log.Printf("cannot handle update event for label %v: no label", event.ID)
			return nil, false
		}
		return nil, u.updateLabel(event.Label)
	case protonmail.EventDelete:
		return u.deleteLabel(event.ID), false
	default:
		log.Printf("cannot handle event for label %v: unknown action %v", event.ID, event.Action)
	}
	return nil, false
}

func (u *user) createLabel(label *protonmail.Label) {
	if !label.HoldsMessages() {
		return
	}

	log.Printf("%v %v %q created", kindNoun(labelKind(label)), label.ID, label.Name)
	// Outside u's lock: newMailbox writes to the database, and u's lock is
	// taken by commands that shouldn't wait for that write.
	mbox, err := newMailbox("", label.ID, nil, u)
	if err != nil {
		log.Printf("cannot create mailbox for label %v: %v", label.ID, err)
		return
	}
	u.Lock()
	defer u.Unlock()
	if _, ok := u.mailboxes[label.ID]; ok {
		// Keep the mailbox clients may have selected.
		return
	}
	mbox.setLabel(label)
	u.mailboxes[label.ID] = mbox
	u.nameMailboxes()
}

func kindNoun(k mailboxKind) string {
	switch k {
	case kindFolder:
		return "Folder"
	case kindLabel:
		return "Label"
	case kindSystem:
		return "System mailbox"
	}
	panic("invariant: unknown mailbox kind")
}

// updateLabel renames the folder or label, moves a folder into another, or
// makes a folder a label or back (Exclusive changed), in place: the mailbox
// keeps its messages, UIDs and selections. A folder's new name renames the
// folders in it, for which Proton sends no event; it reports whether there
// are any, for checkFolderNames. Updates also follow every read or unread
// change in a folder, and then change nothing.
func (u *user) updateLabel(label *protonmail.Label) (renamedParent bool) {
	u.Lock()
	defer u.Unlock()

	mbox, ok := u.mailboxes[label.ID]
	if !ok {
		// A contact group, or a label peroxide never saw created.
		return false
	}
	oldName, oldKind := mbox.Name(), mbox.kind
	mbox.setLabel(label)
	u.nameMailboxes()
	name := mbox.Name()
	if mbox.kind != oldKind {
		log.Printf("%v %v became a %v: %q is now %q", kindNoun(oldKind), label.ID,
			strings.ToLower(kindNoun(mbox.kind)), oldName, name)
	} else if name != oldName {
		log.Printf("%v %v renamed from %q to %q", kindNoun(mbox.kind), label.ID, oldName, name)
	}
	return name != oldName && u.hasChildFolder(label.ID)
}

// checkFolderNames reads every label again after a folder holding others got
// a new name, and takes their names and parents: Proton sends no event for
// the folders inside, and nameMailboxes renamed them from what it knew. A
// difference means Proton changed one without an event, and is logged.
func (u *user) checkFolderNames() {
	labels, err := u.c.ListLabels()
	if err != nil {
		log.Printf("cannot check the names of the folders in a renamed one: %v", err)
		return
	}

	u.Lock()
	defer u.Unlock()
	before := make(map[string]string, len(u.mailboxes))
	for labelID, mbox := range u.mailboxes {
		before[labelID] = mbox.Name()
	}
	for _, label := range labels {
		if mbox, ok := u.mailboxes[label.ID]; ok && mbox.kind != kindSystem && label.HoldsMessages() {
			mbox.setLabel(label)
		}
	}
	u.nameMailboxes()
	for _, labelID := range slices.Sorted(maps.Keys(before)) {
		if name := u.mailboxes[labelID].Name(); name != before[labelID] {
			log.Printf("%v %v renamed from %q to %q without an event", kindNoun(u.mailboxes[labelID].kind), labelID, before[labelID], name)
		}
	}
}

// deleteLabel removes the folder or label, and the folders inside a folder:
// Proton deletes those too, but sends their events later (seen 6 s after,
// 2026-11-09), and until then they would show at the top of Folders. It
// returns a BYE for the clients that have one of them selected, as gluon
// sends ("Mailbox was deleted"): their next command would find no mailbox.
// go-imap sends an update naming a mailbox to those connections only. aerc's
// client closes the connection on a BYE and reconnects, which lists the
// mailboxes again. The later events find nothing to delete.
func (u *user) deleteLabel(labelID string) []imapbackend.Update {
	u.Lock()
	var gone []*mailbox
	seen := make(map[string]bool)
	queue := []string{labelID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		mbox, ok := u.mailboxes[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		gone = append(gone, mbox)
		if mbox.kind == kindFolder {
			for _, childID := range slices.Sorted(maps.Keys(u.mailboxes)) {
				if child := u.mailboxes[childID]; child.kind == kindFolder && child.parentID == id {
					queue = append(queue, childID)
				}
			}
		}
	}
	kinds := make([]mailboxKind, len(gone))
	names := make([]string, len(gone))
	for i, mbox := range gone {
		// Names and kinds before nameMailboxes, which no longer sees them.
		kinds[i], names[i] = mbox.kind, mbox.Name()
		delete(u.mailboxes, mbox.label)
	}
	if len(gone) > 0 {
		u.nameMailboxes()
	}
	u.Unlock()

	var ups []imapbackend.Update
	for i, mbox := range gone {
		log.Printf("%v %v %q deleted", kindNoun(kinds[i]), mbox.label, names[i])
		if err := mbox.delete(); err != nil {
			log.Printf("cannot delete mailbox of label %v: %v", mbox.label, err)
		}
		ups = append(ups, &imapbackend.StatusUpdate{
			Update: imapbackend.NewUpdate(u.u.Name, names[i]),
			StatusResp: &imap.StatusResp{
				Type: imap.StatusRespBye,
				Info: fmt.Sprintf("%v was deleted in Proton: please log in again", names[i]),
			},
		})
	}
	return ups
}
