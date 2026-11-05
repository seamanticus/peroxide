package imap

import (
	"fmt"
	"log"
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
	addrs       []*protonmail.Address

	db             *database.User
	eventsReceiver *events.Receiver
	messages       *messageCache // full messages, safe for concurrent use

	done      chan struct{} // closed by the last Logout
	eventSent chan struct{}

	sync.Mutex // protects everything below

	numClients int
	mailboxes  map[string]*mailbox // indexed by label ID
	flags      map[string]string   // indexed by label ID
}

func getUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList) (*user, error) {
	// TODO: logging a user in may take some time, find a way not to lock all
	// other logins during this time
	be.Lock()
	defer be.Unlock()

	if u, ok := be.users[username]; ok {
		u.Lock()
		u.numClients++
		u.Unlock()
		return u, nil
	} else {
		u, err := newUser(be, username, c, privateKeys)
		if err != nil {
			return nil, err
		}

		be.users[username] = u
		return u, nil
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
		numClients:  1,
		messages:    newMessageCache(),
	}

	db, err := database.Open(u.Name + ".db")
	if err != nil {
		return nil, err
	}
	uu.db = db

	if err := uu.initMailboxes(); err != nil {
		return nil, err
	}

	// Resume after the last event applied, even from a previous session: the
	// events of the time peroxide was stopped are then not lost.
	lastEventID, ok, err := db.LastEventID()
	if err != nil {
		return nil, fmt.Errorf("cannot read last event ID: %w", err)
	}
	if !ok {
		if lastEventID, err = c.GetLatestEventID(); err != nil {
			return nil, fmt.Errorf("cannot get latest event ID: %w", err)
		}
		if err := db.SetLastEventID(lastEventID); err != nil {
			return nil, fmt.Errorf("cannot store last event ID: %w", err)
		}
	}

	uu.done = make(chan struct{})
	ch := make(chan *protonmail.Event)
	go uu.receiveEvents(be.updates, ch)
	uu.eventsReceiver = be.eventsManager.Register(c, u.Name, lastEventID, ch, uu.done)

	log.Printf("User %q logged in via IMAP", u.Name)
	return uu, nil
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

func (u *user) initMailboxes() error {
	u.Lock()
	defer u.Unlock()

	u.mailboxes = make(map[string]*mailbox)
	for _, data := range systemMailboxes {
		var err error
		u.mailboxes[data.label], err = newMailbox(data.name, data.label, data.attrs, u)
		if err != nil {
			return err
		}
	}

	u.flags = make(map[string]string)
	for _, data := range systemFlags {
		u.flags[data.label] = data.name
	}

	labels, err := u.c.ListLabels()
	if err != nil {
		return err
	}

	for _, label := range labels {
		if label.Exclusive == 1 {
			if _, ok := u.mailboxes[label.ID]; ok {
				continue
			}

			u.mailboxes[label.ID], err = newMailbox(label.Name, label.ID, nil, u)
			if err != nil {
				return err
			}
		} else {
			if _, ok := u.flags[label.ID]; ok {
				continue
			}

			u.flags[label.ID] = labelNameToFlag(label.Name)
		}
	}

	counts, err := u.c.CountMessages("")
	if err != nil {
		return err
	}

	for _, count := range counts {
		if mbox, ok := u.mailboxes[count.LabelID]; ok {
			mbox.setAPICounts(count.Total, count.Unread)
		}
	}

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

func (u *user) getFlag(name string) string {
	u.Lock()
	defer u.Unlock()

	for label, flag := range u.flags {
		if flag == name {
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
		panic("unreachable")
	}
	u.numClients--
	if u.numClients > 0 {
		return nil
	}

	delete(u.backend.users, u.username)

	close(u.done)

	if err := u.db.Close(); err != nil {
		return err
	}

	log.Printf("User %q logged out via IMAP", u.u.Name)
	u.c = nil
	u.u = nil
	u.privateKeys = nil
	return nil
}

// poll asks for the events now, and waits until one is applied, or the user
// logged out.
func (u *user) poll() {
	u.eventsReceiver.Poll()
	select {
	case <-u.eventSent:
	case <-u.done:
	}
}

func (u *user) receiveEvents(updates chan<- imapbackend.Update, events <-chan *protonmail.Event) {
	for event := range events {
		var eventUpdates []imapbackend.Update
		var fetchCounts bool

		if event.Refresh&protonmail.EventRefreshMail != 0 {
			log.Println("Reinitializing the whole IMAP database")

			u.Lock()
			for _, mbox := range u.mailboxes {
				if err := mbox.reset(); err != nil {
					log.Printf("cannot reset mailbox %s: %v", mbox.Name(), err)
				}
			}
			u.Unlock()

			if err := u.db.ResetMessages(); err != nil {
				log.Printf("cannot reset user: %v", err)
			}
			u.messages.clear()

			if err := u.initMailboxes(); err != nil {
				log.Printf("cannot reinitialize mailboxes: %v", err)
			}
		} else {
			// First, so that messages moved into a new folder find its mailbox.
			for _, eventLabel := range event.Labels {
				u.handleLabelEvent(eventLabel)
			}

			for _, eventMessage := range event.Messages {
				switch eventMessage.Action {
				case protonmail.EventCreate:
					log.Println("Received create event for message", eventMessage.ID)
					seqNums, err := u.db.CreateMessage(eventMessage.Created)
					if err != nil {
						log.Printf("cannot handle create event for message %s: cannot create message in local DB: %v", eventMessage.ID, err)
						break
					}

					// TODO: what if the message was already in the local DB?
					for labelID, seqNum := range seqNums {
						if mbox := u.getMailboxByLabel(labelID); mbox != nil {
							update := new(imapbackend.MailboxUpdate)
							update.Update = imapbackend.NewUpdate(u.u.Name, mbox.Name())
							update.MailboxStatus = imap.NewMailboxStatus(mbox.Name(), []imap.StatusItem{imap.StatusMessages})
							update.MailboxStatus.Messages = seqNum
							eventUpdates = append(eventUpdates, update)
						}
					}
				case protonmail.EventUpdate, protonmail.EventUpdateFlags:
					log.Println("Received update event for message", eventMessage.ID)
					if eventMessage.Action == protonmail.EventUpdate {
						// Its contents may have changed, e.g. a draft edited.
						u.messages.remove(eventMessage.ID)
					}
					ups, err := u.applyUpdate(eventMessage.ID, eventMessage.Updated)
					eventUpdates = append(eventUpdates, ups...)
					if err != nil {
						log.Printf("cannot handle update event for message %s: %v", eventMessage.ID, err)
					}
				case protonmail.EventDelete:
					log.Println("Received delete event for message", eventMessage.ID)
					ups, err := u.applyDelete(eventMessage.ID)
					eventUpdates = append(eventUpdates, ups...)
					if err != nil {
						log.Printf("cannot handle delete event for message %s: %v", eventMessage.ID, err)
					}
				default:
					// Not a broken invariant: the action comes from the API.
					log.Printf("cannot handle event for message %s: unknown action %v", eventMessage.ID, eventMessage.Action)
				}
			}

			u.setAPICounts(event.MessageCounts)
			// Proton's events carry no message counts: fetch them, as they are
			// all there is for mailboxes not synchronized yet.
			fetchCounts = len(event.MessageCounts) == 0 && len(event.Messages) > 0
		}

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

		// Once applied, so that a page stopped halfway is applied again by the
		// next session: creates, deletes and label changes are idempotent.
		if err := u.db.SetLastEventID(event.ID); err != nil {
			log.Printf("cannot store last event ID %v: %v", event.ID, err)
		}

		// Only now: commands waiting for this event, such as STORE, don't wait
		// for this request too. Synchronized mailboxes don't need it.
		if fetchCounts {
			u.Lock()
			c := u.c // Cleared by Logout, which may race with this event.
			u.Unlock()
			if c != nil {
				counts, err := c.CountMessages("")
				if err != nil {
					log.Printf("cannot refresh message counts after event: %v", err)
				}
				u.setAPICounts(counts)
			}
		}
	}
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
	// Outside u's lock: a database write may wait for read transactions, and
	// SearchMessages takes the lock inside one.
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

// setAPICounts sets the message counts reported by the API.
func (u *user) setAPICounts(counts []*protonmail.MessageCount) {
	for _, count := range counts {
		if mbox := u.getMailboxByLabel(count.LabelID); mbox != nil {
			mbox.setAPICounts(count.Total, count.Unread)
		}
	}
}
