package imap

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/seamanticus/peroxide/events"
	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

var systemMailboxes = []struct {
	name  string
	label string
	attrs []string
}{
	{imap.InboxName, protonmail.LabelInbox, nil},
	{"All Mail", protonmail.LabelAllMail, []string{imap.AllAttr}},
	{"Archive", protonmail.LabelArchive, []string{imap.ArchiveAttr}},
	{"Drafts", protonmail.LabelDraft, []string{imap.DraftsAttr}},
	{"Starred", protonmail.LabelStarred, []string{imap.FlaggedAttr}},
	{"Spam", protonmail.LabelSpam, []string{imap.JunkAttr}},
	{"Sent", protonmail.LabelSent, []string{imap.SentAttr}},
	{"Trash", protonmail.LabelTrash, []string{imap.TrashAttr}},
}

var systemFlags = []struct {
	name  string
	label string
}{
	{imap.FlaggedFlag, protonmail.LabelStarred},
	{imap.DraftFlag, protonmail.LabelDraft},
}

type user struct {
	username    string
	backend     *backend
	c           *protonmail.Client
	u           *protonmail.User
	privateKeys openpgp.EntityList

	db             *database.User
	eventsReceiver *events.Receiver
	messages       *messageCache // full messages, safe for concurrent use

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
	// syncSem serializes what changes the stored messages and mailboxes: a
	// page of the download (asked for and stored), an event page, an action
	// applied without waiting for its event, a rebuild (lockSync,
	// unlockSync). A page of the download fetched before an event and stored
	// after it would undo it. A 1-slot channel rather than a sync.Mutex, so
	// that a goroutine waiting on it is durably blocked for testing/synctest.
	// Taken before u's and the mailboxes' locks, and never inside a database
	// transaction.
	syncSem chan struct{}

	sync.Mutex // protects everything below

	numClients int
	// Set by rebuild: no login joins this user, which is ending.
	rebuilding bool
	mailboxes  map[string]*mailbox // indexed by label ID
	flags      map[string]string   // indexed by label ID
	// Replaced, never changed in place, by Addresses events: read it with
	// addresses.
	addrs []*protonmail.Address
}

func getUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList) (*user, error) {
	for {
		// TODO: logging a user in may take some time, find a way not to lock all
		// other logins during this time
		be.Lock()
		u, ok := be.users[username]
		if !ok {
			u, err := newUser(be, username, c, privateKeys)
			if err != nil {
				be.Unlock()
				return nil, err
			}
			be.users[username] = u
			be.Unlock()
			return u, nil
		}

		u.Lock()
		// A user being rebuilt is closing too: its clients are being
		// disconnected, and the login that follows wipes the database.
		closing := u.numClients == 0 || u.rebuilding
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

func newUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList) (*user, error) {
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
		numClients:  1,
		messages:    newMessageCache(),
	}

	db, err := database.Open(u.Name + ".db")
	if err != nil {
		return nil, err
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
	// Before the events goroutine, which owns downloadStop from then on.
	uu.startDownload()
	ch := make(chan *protonmail.Event)
	uu.startEvents(be.updates, ch)
	uu.eventsReceiver = be.eventsManager.Register(c, u.Name, lastEventID, ch, uu.done)

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

func labelNameToFlag(s string) string {
	var sb strings.Builder
	var lastValid bool
	for _, r := range s {
		// See atom-specials in RFC 3501
		var valid bool
		switch r {
		case '(', ')', '{':
		case ' ', '\t': // SP
		case '%', '*': // list-wildcards
		case '"', '\\': // quoted-specials
		case ']': // resp-specials
		default:
			valid = r <= '~' && r > 31
		}
		if !valid {
			if !lastValid {
				continue
			}
			r = '_'
		}
		sb.WriteRune(r)
		lastValid = valid
	}
	return sb.String()
}

// setMailboxes makes the mailboxes and flags of the system labels and of
// labels, creating their buckets if the database has none.
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

	flags := make(map[string]string)
	for _, data := range systemFlags {
		flags[data.label] = data.name
	}

	for _, label := range labels {
		if label.Exclusive == 1 {
			if _, ok := mailboxes[label.ID]; ok {
				continue
			}

			var err error
			mailboxes[label.ID], err = newMailbox(label.Name, label.ID, nil, u)
			if err != nil {
				return err
			}
		} else {
			if _, ok := flags[label.ID]; ok {
				continue
			}

			flags[label.ID] = labelNameToFlag(label.Name)
		}
	}

	u.mailboxes, u.flags = mailboxes, flags
	return nil
}

func (u *user) Username() string {
	return u.u.Name
}

func (u *user) ListMailboxes(subscribed bool) ([]imapbackend.Mailbox, error) {
	u.Lock()
	defer u.Unlock()

	list := make([]imapbackend.Mailbox, 0, len(u.mailboxes))
	for _, mbox := range u.mailboxes {
		// Hidden from LIST only, as proton-bridge hides it (gluon's
		// GetMailboxVisibility): SELECT and STATUS still find it by name.
		if mbox.label == protonmail.LabelAllMail && !u.backend.showAllMail {
			continue
		}
		list = append(list, mbox)
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

	for _, mbox := range u.mailboxes {
		if mbox.Name() == name {
			return mbox
		}
	}
	return nil
}

func (u *user) GetMailbox(name string) (imapbackend.Mailbox, error) {
	mbox := u.getMailbox(name)
	if mbox == nil {
		return nil, imapbackend.ErrNoSuchMailbox
	}
	return mbox, nil
}

// getFlag returns the label whose flag is name. Flags are case-insensitive,
// and go-imap hands over every flag RFC 3501 doesn't define in lower case
// (imap.CanonicalFlag) while label flags keep their case. Labels are tried in
// ID order, so two that differ only in case resolve the same way every time.
func (u *user) getFlag(name string) string {
	u.Lock()
	defer u.Unlock()

	for _, label := range slices.Sorted(maps.Keys(u.flags)) {
		if strings.EqualFold(u.flags[label], name) {
			return label
		}
	}
	return ""
}

func (u *user) CreateMailbox(name string) error {
	return errNotYetImplemented // TODO
}

func (u *user) DeleteMailbox(name string) error {
	return errNotYetImplemented // TODO
}

func (u *user) RenameMailbox(existingName, newName string) error {
	return errNotYetImplemented // TODO
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
	for event := range events {
		if rebuilding {
			continue
		}
		var eventUpdates []imapbackend.Update
		var failed int

		// Applied whole while no download page is stored, but sent after:
		// sending waits for go-imap, which mustn't wait for the download.
		u.lockSync()
		refresh := event.Refresh&protonmail.EventRefreshMail != 0
		if !refresh {
			// First, so that messages moved into a new folder find its mailbox.
			for _, eventLabel := range event.Labels {
				u.handleLabelEvent(eventLabel)
			}
			for _, eventAddress := range event.Addresses {
				u.handleAddressEvent(eventAddress)
			}

			for _, eventMessage := range event.Messages {
				ups, err := u.applyMessageEvent(eventMessage)
				eventUpdates = append(eventUpdates, ups...)
				if err != nil {
					log.Printf("cannot handle event for message %s: %v", eventMessage.ID, err)
					failed++
				}
			}
		}
		u.unlockSync()

		for _, update := range eventUpdates {
			// go-imap creates Done's channel lazily without synchronization, and
			// both the server and the goroutine below call it: create it first.
			update.Done()
			updates <- update
		}
		go func() {
			for _, update := range eventUpdates {
				<-update.Done()
			}

			select {
			case u.eventSent <- struct{}{}:
			default:
			}
		}()

		if refresh || failed > 0 {
			if refresh {
				log.Printf("Proton asked %q to refresh its mail: rebuilding the database", u.u.Name)
			} else {
				log.Printf("%v message events of %q could not be applied: rebuilding the database", failed, u.u.Name)
			}
			u.rebuild(updates)
			rebuilding = true
			continue
		}

		// Once applied, so that a page stopped halfway is applied again by the
		// next session: creates, deletes and label changes are idempotent.
		if err := u.db.SetLastEventID(event.ID); err != nil {
			log.Printf("cannot store last event ID %v: %v", event.ID, err)
		}
	}
}

// applyMessageEvent applies one message event to the database, and returns
// the updates telling clients about it. An error means the database may now
// differ from Proton.
func (u *user) applyMessageEvent(eventMessage *protonmail.EventMessage) ([]imapbackend.Update, error) {
	switch eventMessage.Action {
	case protonmail.EventCreate:
		log.Println("Received create event for message", eventMessage.ID)
		// A message the download listed first is updated, keeping its UIDs.
		change, err := u.db.CreateMessage(eventMessage.Created)
		if err != nil {
			return nil, fmt.Errorf("cannot create message in local DB: %w", err)
		}
		return u.downloadUpdates([]database.MessageChange{change}), nil
	case protonmail.EventUpdate, protonmail.EventUpdateFlags:
		log.Println("Received update event for message", eventMessage.ID)
		if eventMessage.Action == protonmail.EventUpdate {
			// Its contents may have changed, e.g. a draft edited.
			u.messages.remove(eventMessage.ID)
		}
		ups, err := u.applyUpdate(eventMessage.ID, eventMessage.Updated)
		if errors.Is(err, database.ErrNotFound) {
			return ups, u.updateUnknownMessage(eventMessage.ID)
		}
		return ups, err
	case protonmail.EventDelete:
		log.Println("Received delete event for message", eventMessage.ID)
		return u.applyDelete(eventMessage.ID)
	default:
		// Not a broken invariant: the action comes from the API. Nothing is
		// known to be lost either.
		log.Printf("cannot handle event for message %s: unknown action %v", eventMessage.ID, eventMessage.Action)
		return nil, nil
	}
}

// updateUnknownMessage handles an update event for a message the database
// lacks. While the download runs it will list the message, as Proton has it
// then. After, the message should be there: it is fetched and created, as
// proton-bridge creates a message an update finds missing.
func (u *user) updateUnknownMessage(apiID string) error {
	_, done, err := u.db.SyncState()
	if err != nil {
		return fmt.Errorf("cannot read where the download stands: %w", err)
	}
	if !done {
		return nil
	}
	msgs, err := u.c.MessagesByID([]string{apiID})
	if err != nil {
		return fmt.Errorf("cannot get the message missing from the local DB: %w", err)
	}
	for _, msg := range msgs {
		if _, err := u.db.CreateMessage(msg); err != nil {
			return fmt.Errorf("cannot create the message missing from the local DB: %w", err)
		}
	}
	return nil
}

// rebuild marks the database for a rebuild, stops the download and
// disconnects every client of the account, with BYE, as proton-bridge removes
// its IMAP user on a Refresh or a bad event. The login that follows wipes the
// database and downloads the account again (initSession); peroxide stopped
// before then does it on its next login. Called by the events goroutine only,
// without syncSem.
func (u *user) rebuild(updates chan<- imapbackend.Update) {
	u.Lock()
	u.rebuilding = true
	u.Unlock()
	if err := u.db.SetNeedsRebuild(); err != nil {
		// The page's event ID isn't stored either: the next session applies
		// it again, and fails again or succeeds.
		log.Printf("cannot mark the database of %q for a rebuild: %v", u.u.Name, err)
	}
	u.stopDownload()

	bye := &imapbackend.StatusUpdate{
		Update: imapbackend.NewUpdate(u.u.Name, ""),
		StatusResp: &imap.StatusResp{
			Type: imap.StatusRespBye,
			Info: "Proton's mail changed in a way peroxide must download again: please log in again",
		},
	}
	bye.Done()
	updates <- bye
	<-bye.Done()
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
// Proton, as initMailboxes would. Clients see it on their next LIST.
func (u *user) handleLabelEvent(event *protonmail.EventLabel) {
	// Proton updates system labels after every read or unread change, with
	// names and Exclusive values that don't match their mailboxes and flags.
	// Their events have IDs of their own: only Label holds the label ID.
	labelID := event.ID
	if event.Label != nil {
		labelID = event.Label.ID
	}
	if isSystemLabel(labelID) {
		return
	}

	switch event.Action {
	case protonmail.EventCreate:
		if event.Label == nil {
			log.Printf("cannot handle create event for label %v: no label", event.ID)
			return
		}
		u.createLabel(event.Label)
	case protonmail.EventUpdate, protonmail.EventUpdateFlags:
		if event.Label == nil {
			log.Printf("cannot handle update event for label %v: no label", event.ID)
			return
		}
		u.updateLabel(event.Label)
	case protonmail.EventDelete:
		u.deleteLabel(event.ID)
	default:
		log.Printf("cannot handle event for label %v: unknown action %v", event.ID, event.Action)
	}
}

func (u *user) createLabel(label *protonmail.Label) {
	if label.Type == protonmail.LabelContact {
		return
	}

	if label.Exclusive != 1 {
		log.Printf("Label %v %q created", label.ID, label.Name)
		u.Lock()
		u.flags[label.ID] = labelNameToFlag(label.Name)
		u.Unlock()
		return
	}

	log.Printf("Folder %v %q created", label.ID, label.Name)
	// Outside u's lock: newMailbox writes to the database, and u's lock is
	// taken by commands that shouldn't wait for that write.
	mbox, err := newMailbox(label.Name, label.ID, nil, u)
	if err != nil {
		log.Printf("cannot create mailbox for folder %v: %v", label.ID, err)
		return
	}
	u.Lock()
	if _, ok := u.mailboxes[label.ID]; !ok {
		u.mailboxes[label.ID] = mbox
	}
	u.Unlock()
}

// updateLabel renames the folder or label. Updates also follow every read or
// unread change in it, and then change nothing.
func (u *user) updateLabel(label *protonmail.Label) {
	u.Lock()
	defer u.Unlock()

	if mbox, ok := u.mailboxes[label.ID]; ok {
		if label.Exclusive != 1 {
			log.Printf("Folder %v became a label: ignored until the next login", label.ID)
			return
		}
		if old := mbox.Name(); old != label.Name {
			log.Printf("Folder %v renamed from %q to %q", label.ID, old, label.Name)
			mbox.setName(label.Name)
		}
		return
	}

	if flag, ok := u.flags[label.ID]; ok {
		if label.Exclusive == 1 {
			log.Printf("Label %v became a folder: ignored until the next login", label.ID)
			return
		}
		if name := labelNameToFlag(label.Name); name != flag {
			log.Printf("Label %v renamed from %q to %q", label.ID, flag, name)
			u.flags[label.ID] = name
		}
	}
	// Anything else, such as a contact group, has no mailbox or flag.
}

func (u *user) deleteLabel(labelID string) {
	u.Lock()
	mbox, isFolder := u.mailboxes[labelID]
	delete(u.mailboxes, labelID)
	delete(u.flags, labelID)
	u.Unlock()

	if !isFolder {
		return
	}
	log.Printf("Folder %v %q deleted", labelID, mbox.Name())
	if err := mbox.delete(); err != nil {
		log.Printf("cannot delete mailbox of folder %v: %v", labelID, err)
	}
}
