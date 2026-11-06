package imap

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

const delimiter = "/"

type mailbox struct {
	label string
	attrs []string

	u  *user
	db *database.Mailbox

	// The name changes when the folder is renamed in Proton, and go-imap reads
	// it from other goroutines: read it with Name.
	nameLock sync.Mutex
	name     string

	// Message counts reported by the API, the only ones known until the
	// mailbox is synchronized. They have their own lock, as init holds the
	// one below for as long as synchronizing takes.
	apiCountsLock       sync.Mutex
	apiTotal, apiUnread int

	sync.Mutex // protects everything below

	initialized bool
	deleted     map[string]struct{}
}

func newMailbox(name string, label string, attrs []string, u *user) (*mailbox, error) {
	mboxDB, err := u.db.Mailbox(label)
	if err != nil {
		return nil, err
	}

	return &mailbox{
		name:    name,
		label:   label,
		attrs:   attrs,
		u:       u,
		db:      mboxDB,
		deleted: make(map[string]struct{}),
	}, nil
}

func (mbox *mailbox) Name() string {
	mbox.nameLock.Lock()
	defer mbox.nameLock.Unlock()

	return mbox.name
}

func (mbox *mailbox) setName(name string) {
	mbox.nameLock.Lock()
	defer mbox.nameLock.Unlock()

	mbox.name = name
}

func (mbox *mailbox) Info() (*imap.MailboxInfo, error) {
	return &imap.MailboxInfo{
		Attributes: append([]string{imap.NoInferiorsAttr}, mbox.attrs...),
		Delimiter:  delimiter,
		Name:       mbox.Name(),
	}, nil
}

func (mbox *mailbox) setAPICounts(total, unread int) {
	mbox.apiCountsLock.Lock()
	defer mbox.apiCountsLock.Unlock()

	mbox.apiTotal, mbox.apiUnread = total, unread
}

func (mbox *mailbox) apiCounts() (total, unread int) {
	mbox.apiCountsLock.Lock()
	defer mbox.apiCountsLock.Unlock()

	return mbox.apiTotal, mbox.apiUnread
}

func (mbox *mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	mbox.u.Lock()
	flags := []string{imap.SeenFlag, imap.DeletedFlag}
	permFlags := []string{imap.SeenFlag, imap.DeletedFlag}
	for _, label := range slices.Sorted(maps.Keys(mbox.u.flags)) {
		// By label ID, not map order: FLAGS and PERMANENTFLAGS read the same
		// on every SELECT.
		flags = append(flags, mbox.u.flags[label])
		permFlags = append(permFlags, mbox.u.flags[label])
	}
	mbox.u.Unlock()

	status := imap.NewMailboxStatus(mbox.Name(), items)
	status.Flags = flags
	status.PermanentFlags = permFlags
	status.UnseenSeqNum = 0 // TODO

	mbox.Lock()
	defer mbox.Unlock()

	// The local DB, kept up to date by events, is exact once synchronized;
	// until then only the API's counts are known.
	var total, unread int
	if mbox.initialized {
		var err error
		total, unread, err = mbox.db.Count()
		if err != nil {
			return nil, fmt.Errorf("cannot count messages in mailbox %v: %w", mbox.Name(), err)
		}
	} else {
		total, unread = mbox.apiCounts()
	}

	for _, name := range items {
		switch name {
		case imap.StatusMessages:
			status.Messages = uint32(total)
		case imap.StatusUidNext:
			uidNext, err := mbox.db.UidNext()
			if err != nil {
				return nil, err
			}
			status.UidNext = uidNext
		case imap.StatusUidValidity:
			status.UidValidity = 1
		case imap.StatusRecent:
			status.Recent = 0
		case imap.StatusUnseen:
			status.Unseen = uint32(unread)
		case imap.StatusAppendLimit:
			status.AppendLimit = appendLimit
		}
	}

	return status, nil
}

func (mbox *mailbox) SetSubscribed(subscribed bool) error {
	return errNotYetImplemented // TODO
}

func (mbox *mailbox) Check() error {
	return nil
}

func (mbox *mailbox) sync() error {
	log.Printf("Synchronizing mailbox %v...", mbox.Name())

	// TODO: don't do this without incrementing UIDVALIDITY
	if err := mbox.db.Reset(); err != nil {
		return err
	}

	filter := &protonmail.MessageFilter{
		PageSize: 150,
		Label:    mbox.label,
		Sort:     "ID",
		Asc:      true,
	}

	total := -1
	for {
		offset := filter.PageSize * filter.Page
		if total >= 0 && offset > total {
			break
		}

		var page []*protonmail.Message
		var err error
		total, page, err = mbox.u.c.ListMessages(filter)
		if err != nil {
			return err
		}

		if err := mbox.db.Sync(page); err != nil {
			return err
		}

		filter.Page++
	}

	log.Printf("Synchronizing mailbox %v: done.", mbox.Name())
	return nil
}

func (mbox *mailbox) init() error {
	mbox.Lock()
	initialized := mbox.initialized
	mbox.Unlock()
	if initialized {
		return nil
	}

	// Taken before mbox's lock (user.syncSem): no event page is applied while
	// the sync has emptied the mailbox, so none of its messages gets a UID
	// before the older ones.
	mbox.u.lockSync()
	defer mbox.u.unlockSync()
	mbox.Lock()
	defer mbox.Unlock()

	if mbox.initialized { // synchronized while this one waited
		return nil
	}

	// TODO: sync only the first time (TODO.md section 9, step 6)
	if err := mbox.sync(); err != nil {
		return err
	}

	mbox.initialized = true
	return nil
}

func (mbox *mailbox) reset() error {
	mbox.Lock()
	defer mbox.Unlock()

	mbox.initialized = false
	return mbox.db.Reset()
}

// delete forgets the mailbox after its folder was deleted. Clients that still
// have it selected get errors from then on. A write that grows the database
// file waits for open read transactions, so no reader may wait for a lock
// inside one (SearchMessages and flagsUpdates collect entries first): that
// keeps writes under mbox's lock, as sync's, safe. The bucket is deleted
// after the lock is released all the same, as nothing needs it held.
// Called while an event page holds user.syncSem, so no sync refills the
// bucket meanwhile.
func (mbox *mailbox) delete() error {
	mbox.Lock()
	mbox.initialized = false
	mbox.Unlock()

	return mbox.db.Delete()
}

func (mbox *mailbox) fetchFlags(msg *protonmail.Message) []string {
	var flags []string
	if msg.Unread != 1 {
		flags = append(flags, imap.SeenFlag)
	}
	if msg.IsReplied != 0 || msg.IsRepliedAll != 0 {
		flags = append(flags, imap.AnsweredFlag)
	}

	mbox.Lock()
	if _, ok := mbox.deleted[msg.ID]; ok {
		flags = append(flags, imap.DeletedFlag)
	}
	mbox.Unlock()

	mbox.u.Lock()
	for _, label := range msg.LabelIDs {
		if flag, ok := mbox.u.flags[label]; ok {
			flags = append(flags, flag)
		}
	}
	mbox.u.Unlock()

	return flags
}

func (mbox *mailbox) fetchMessage(isUid bool, id uint32, items []imap.FetchItem) (*imap.Message, *protonmail.Message, error) {
	var apiID string
	var err error
	if isUid {
		apiID, err = mbox.db.FromUid(id)
	} else {
		apiID, err = mbox.db.FromSeqNum(id)
	}
	if err != nil {
		return nil, nil, err
	}

	seqNum, uid, err := mbox.db.FromApiID(apiID)
	if err != nil {
		return nil, nil, err
	}

	msg, err := mbox.u.db.Message(apiID)
	if err != nil {
		return nil, nil, err
	}

	fetched := imap.NewMessage(seqNum, items)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			fetched.Envelope = fetchEnvelope(msg)
		case imap.FetchBody, imap.FetchBodyStructure:
			bs, err := mbox.fetchBodyStructure(msg, item == imap.FetchBodyStructure)
			if err != nil {
				return nil, nil, err
			}
			fetched.BodyStructure = bs
		case imap.FetchFlags:
			fetched.Flags = mbox.fetchFlags(msg)
		case imap.FetchInternalDate:
			fetched.InternalDate = msg.Time.Time()
		case imap.FetchRFC822Size:
			fetched.Size = uint32(msg.Size)
		case imap.FetchUid:
			fetched.Uid = uid
		case imap.FetchRFC822, imap.FetchRFC822Header, imap.FetchRFC822Text:
			// Old names of BODY[], BODY.PEEK[HEADER] and BODY[TEXT].
			if err := mbox.fetchSection(fetched, msg, item); err != nil {
				return nil, nil, err
			}
		case imap.FetchAll, imap.FetchFast, imap.FetchFull:
			panic("invariant: ListMessages expands FETCH macros")
		default:
			// BODY[...] and BODY.PEEK[...], which name any section.
			if err := mbox.fetchSection(fetched, msg, item); err != nil {
				return nil, nil, err
			}
		}
	}

	return fetched, msg, nil
}

// fetchSection adds the body section item names to fetched. An item that
// names no section is an error: answering without it would drop it silently.
func (mbox *mailbox) fetchSection(fetched *imap.Message, msg *protonmail.Message, item imap.FetchItem) error {
	section, err := imap.ParseBodySectionName(item)
	if err != nil {
		return fmt.Errorf("unknown FETCH item %v: %w", item, err)
	}

	l, err := mbox.fetchBodySection(msg, section)
	if err != nil {
		return err
	}
	fetched.Body[section] = l
	return nil
}

// expandFetchItems replaces the FETCH macros ALL, FAST and FULL in items by
// what they stand for (RFC 3501 section 6.4.5). go-imap's parser does so too,
// but fetchMessage must not depend on it.
func expandFetchItems(items []imap.FetchItem) []imap.FetchItem {
	var expanded []imap.FetchItem
	for _, item := range items {
		expanded = append(expanded, item.Expand()...)
	}
	return expanded
}

// setsSeen reports whether fetching items sets \Seen (RFC 3501 section
// 6.4.5): a body section without .PEEK, which RFC822 and RFC822.TEXT are.
func setsSeen(items []imap.FetchItem) bool {
	for _, item := range items {
		if section, err := imap.ParseBodySectionName(item); err == nil && !section.Peek {
			return true
		}
	}
	return false
}

// ListMessages is FETCH as go-imap's own handler calls it, which can't say
// whether the mailbox was opened read-only: it never sets \Seen, so that it
// can't set it where RFC 3501 forbids it. Extension's FETCH, which knows,
// calls fetch instead.
func (mbox *mailbox) ListMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	return mbox.fetch(uid, seqSet, items, ch, false)
}

// fetch sends the messages of seqSet with items to ch, and closes it. With
// markRead, the unread ones it sends a body section of without .PEEK get
// \Seen (RFC 3501 section 6.4.5); markRead is false in a mailbox opened
// read-only (section 6.3.2).
func (mbox *mailbox) fetch(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message, markRead bool) error {
	opened, err := mbox.listMessages(uid, seqSet, items, ch)
	if err != nil {
		return err
	}
	if !markRead || len(opened) == 0 {
		return nil
	}

	// Only once ch is closed: until then go-imap is writing the FETCH response,
	// and the FETCH telling this client about \Seen would wait behind it.
	failed, err := mbox.u.c.MarkMessagesRead(opened)
	if err == nil {
		err = mbox.u.applyAction(opened, failed, unreadUpdate(0))
	}
	if err != nil {
		// The client has the messages: failing FETCH would have it fetch
		// them again for a flag it can still set with STORE.
		log.Printf("cannot set \\Seen on messages fetched from %v: %v", mbox.Name(), err)
	}
	return nil
}

// listMessages sends the messages of seqSet to ch, and closes it. It returns
// the unread messages among them that fetching items opens, as setsSeen says.
func (mbox *mailbox) listMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) (opened []string, err error) {
	defer close(ch)

	if err := mbox.init(); err != nil {
		return nil, err
	}

	// Every message is fetched with the same items, so an unknown one fails
	// the first message found, before any is sent.
	items = expandFetchItems(items)
	opens := setsSeen(items)

	for _, seq := range seqSet.Set {
		start := seq.Start
		if start == 0 {
			start = 1
		}

		stop := seq.Stop
		if stop == 0 {
			if uid {
				uidNext, err := mbox.db.UidNext()
				if err != nil {
					return nil, err
				}
				stop = uidNext - 1
			} else {
				total, _, err := mbox.db.Count()
				if err != nil {
					return nil, fmt.Errorf("cannot count messages in mailbox %v: %w", mbox.Name(), err)
				}
				stop = uint32(total)
			}
		}

		for i := start; i <= stop; i++ {
			fetched, msg, err := mbox.fetchMessage(uid, i, items)
			if errors.Is(err, errMessageGone) {
				// Its delete event removes it, with an EXPUNGE, at the next
				// poll: failing the FETCH would also drop the messages after it.
				log.Printf("Message %v of %v skipped: %v", i, mbox.Name(), err)
				continue
			} else if errors.Is(err, database.ErrNotFound) {
				continue
			} else if err != nil {
				return nil, err
			}
			ch <- fetched
			if opens && msg.Unread == 1 {
				opened = append(opened, msg.ID)
			}
		}
	}

	return opened, nil
}

func matchString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func (mbox *mailbox) matchMessage(seqNum, uid uint32, msg *protonmail.Message, c *imap.SearchCriteria) bool {
	if c.SeqNum != nil && !c.SeqNum.Contains(seqNum) {
		return false
	}
	if c.Uid != nil && !c.Uid.Contains(uid) {
		return false
	}

	flags := make(map[string]bool)
	for _, flag := range mbox.fetchFlags(msg) {
		flags[flag] = true
	}
	for _, f := range c.WithFlags {
		if !flags[f] {
			return false
		}
	}
	for _, f := range c.WithoutFlags {
		if flags[f] {
			return false
		}
	}

	date := msg.Time.Time().Round(24 * time.Hour)
	if !c.Since.IsZero() && !date.After(c.Since) {
		return false
	}

	if !c.Before.IsZero() && !date.Before(c.Before) {
		return false
	}

	// TODO: this date should be from the Date MIME header
	if !c.SentBefore.IsZero() && !date.Before(c.SentBefore) {
		return false
	}

	if !c.SentSince.IsZero() && !date.After(c.SentSince) {
		return false
	}

	if c.Larger > 0 && uint32(msg.Size) < c.Larger {
		return false
	}

	if c.Smaller > 0 && uint32(msg.Size) > c.Smaller {
		return false
	}

	h := messageHeader(msg)
	for key, wantValues := range c.Header {
		fields := h.FieldsByKey(key)
		var values []string
		for fields.Next() {
			values = append(values, fields.Value())
		}

		for _, wantValue := range wantValues {
			if wantValue == "" && len(values) == 0 {
				return false
			}
			if wantValue != "" {
				ok := false
				for _, v := range values {
					if matchString(v, wantValue) {
						ok = true
						break
					}
				}
				if !ok {
					return false
				}
			}
		}
	}

	for _, e := range c.Body {
		if !matchString(msg.Body, e) {
			return false
		}
	}

	for _, e := range c.Text {
		if !matchString(msg.Body, e) && !matchString(msg.Header, e) {
			return false
		}
	}

	for _, e := range c.Or {
		if !mbox.matchMessage(seqNum, uid, msg, e[0]) && !mbox.matchMessage(seqNum, uid, msg, e[1]) {
			return false
		}
	}

	for _, e := range c.Not {
		if mbox.matchMessage(seqNum, uid, msg, e) {
			return false
		}
	}

	return true
}

func (mbox *mailbox) SearchMessages(isUID bool, c *imap.SearchCriteria) ([]uint32, error) {
	if err := mbox.init(); err != nil {
		return nil, err
	}

	// The entries are collected in one read transaction, and the messages
	// read and matched after it ends. matchMessage takes locks (fetchFlags),
	// and a bolt write that grows the file waits for every open read
	// transaction: matching inside one deadlocked against a writer holding,
	// or waiting for, those locks. A nested read transaction did as well.
	type entry struct {
		seqNum, uid uint32
		apiID       string
	}
	var entries []entry
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		entries = append(entries, entry{seqNum, uid, apiID})
		return nil
	})
	if err != nil {
		return nil, err
	}

	var results []uint32
	for _, e := range entries {
		// TODO: fetch message from local DB only if needed
		msg, err := mbox.u.db.Message(e.apiID)
		if errors.Is(err, database.ErrNotFound) {
			continue // deleted by an event since the entries were read
		} else if err != nil {
			return nil, err
		}

		if mbox.matchMessage(e.seqNum, e.uid, msg, c) {
			if isUID {
				results = append(results, e.uid)
			} else {
				results = append(results, e.seqNum)
			}
		}
	}
	return results, nil
}

func (mbox *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if body.Len() > appendLimit {
		return imapbackend.ErrTooBig
	}
	if mbox.label != protonmail.LabelDraft {
		return errors.New("cannot create messages outside the Drafts mailbox")
	}

	if err := mbox.init(); err != nil {
		return err
	}

	_, err := createMessage(mbox.u.c, mbox.u.u, mbox.u.privateKeys, mbox.u.addresses(), body)
	if err != nil {
		return err
	}

	return mbox.Poll()
}

func (mbox *mailbox) fromSeqSet(isUID bool, seqSet *imap.SeqSet) ([]string, error) {
	var apiIDs []string
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		var id uint32
		if isUID {
			id = uid
		} else {
			id = seqNum
		}

		if seqSet.Contains(id) {
			apiIDs = append(apiIDs, apiID)
		}
		return nil
	})
	return apiIDs, err
}

func (mbox *mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	switch op {
	case imap.AddFlags, imap.RemoveFlags:
		for _, flag := range flags {
			if err := mbox.applyFlag(apiIDs, flag, op == imap.AddFlags); err != nil {
				return err
			}
		}
		return nil
	case imap.SetFlags:
		// RFC 3501 section 6.4.6: FLAGS replaces the flags. Read who holds
		// what first: adding the listed flags changes it.
		holders, err := mbox.flagHolders(apiIDs)
		if err != nil {
			return err
		}
		for _, flag := range flags {
			if err := mbox.applyFlag(apiIDs, flag, true); err != nil {
				return err
			}
		}
		for _, flag := range slices.Sorted(maps.Keys(holders)) {
			// go-imap lowercases flags RFC 3501 doesn't define, and label
			// flags keep their case: compare without it, or a label asked
			// for would be removed.
			if slices.ContainsFunc(flags, func(f string) bool { return strings.EqualFold(f, flag) }) {
				continue
			}
			if err := mbox.applyFlag(holders[flag], flag, false); err != nil {
				return err
			}
		}
		return nil
	default:
		panic(fmt.Sprintf("invariant: unknown flags operation %q", op))
	}
}

// applyFlag adds flag to, or removes it from, the messages apiIDs. Flags that
// stand for nothing in Proton, such as \Answered and \Draft, are left alone.
func (mbox *mailbox) applyFlag(apiIDs []string, flag string, add bool) error {
	switch flag {
	case imap.SeenFlag:
		var failed map[string]error
		var err error
		unread := 0
		if add {
			failed, err = mbox.u.c.MarkMessagesRead(apiIDs)
		} else {
			failed, err = mbox.u.c.MarkMessagesUnread(apiIDs)
			unread = 1
		}
		if err != nil {
			return err
		}
		return mbox.u.applyAction(apiIDs, failed, unreadUpdate(unread))
	case imap.DeletedFlag:
		// \Deleted is kept by the mailbox only, for its EXPUNGE: Proton has
		// no such flag, so it is never sent there and is lost when peroxide
		// restarts, which RFC 3501 allows. Every connection with the mailbox
		// selected shares it and is told; go-imap drops the FETCH for one
		// that sent .SILENT.
		mbox.Lock()
		for _, apiID := range apiIDs {
			if add {
				mbox.deleted[apiID] = struct{}{}
			} else {
				delete(mbox.deleted, apiID)
			}
		}
		mbox.Unlock()
		updates, err := mbox.flagsUpdates(apiIDs)
		if err != nil {
			return err
		}
		mbox.u.sendUpdates(updates)
		return nil
	case imap.DraftFlag:
		// Drafts are a mailbox in Proton, not a label a message can be given.
		return nil
	default:
		label := mbox.u.getFlag(flag)
		if label == "" {
			return nil
		}
		var failed map[string]error
		var err error
		if add {
			failed, err = mbox.u.c.LabelMessages(label, apiIDs)
		} else {
			failed, err = mbox.u.c.UnlabelMessages(label, apiIDs)
		}
		if err != nil {
			return err
		}
		return mbox.u.applyAction(apiIDs, failed, labelUpdate(label, add))
	}
}

// flagHolders returns, for each flag one of the messages apiIDs has, the
// messages that have it, in the order of apiIDs.
func (mbox *mailbox) flagHolders(apiIDs []string) (map[string][]string, error) {
	holders := make(map[string][]string)
	for _, apiID := range apiIDs {
		msg, err := mbox.u.db.Message(apiID)
		if errors.Is(err, database.ErrNotFound) {
			continue // deleted by an event since the IDs were read
		} else if err != nil {
			return nil, err
		}
		for _, flag := range mbox.fetchFlags(msg) {
			holders[flag] = append(holders[flag], apiID)
		}
	}
	return holders, nil
}

// flagsUpdates returns a FETCH of the flags of each message of apiIDs in this
// mailbox, with its UID, as clients may have asked with a UID command and only
// take responses bearing it (RFC 3501 section 6.4.8).
func (mbox *mailbox) flagsUpdates(apiIDs []string) ([]imapbackend.Update, error) {
	want := make(map[string]struct{}, len(apiIDs))
	for _, apiID := range apiIDs {
		want[apiID] = struct{}{}
	}
	// The entries are read in one transaction and the messages after it ends,
	// as in SearchMessages: a nested read transaction deadlocks with writers.
	type entry struct {
		seqNum, uid uint32
		apiID       string
	}
	var entries []entry
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		if _, ok := want[apiID]; ok {
			entries = append(entries, entry{seqNum, uid, apiID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var updates []imapbackend.Update
	for _, e := range entries {
		msg, err := mbox.u.db.Message(e.apiID)
		if errors.Is(err, database.ErrNotFound) {
			continue // deleted by an event since the entries were read
		} else if err != nil {
			return nil, err
		}
		up := new(imapbackend.MessageUpdate)
		up.Update = newMailboxUpdate(mbox.u.u.Name, mbox)
		up.Message = imap.NewMessage(e.seqNum, []imap.FetchItem{imap.FetchFlags, imap.FetchUid})
		up.Message.Flags = mbox.fetchFlags(msg)
		up.Message.Uid = e.uid
		updates = append(updates, up)
	}
	return updates, nil
}

func (mbox *mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	dest := mbox.u.getMailbox(destName)
	if dest == nil {
		return imapbackend.ErrNoSuchMailbox
	}

	failed, err := mbox.u.c.LabelMessages(dest.label, apiIDs)
	if err != nil {
		return err
	}
	return mbox.u.applyAction(apiIDs, failed, labelUpdate(dest.label, true))
}

// MoveMessages labels the messages with dest, then unlabels them from mbox. Each
// message must end in one of the two, and should not end in both, even when
// the command fails (RFC 6851 section 3.3): one dest's label failed for is
// left alone, and one the unlabel failed for is checked and taken out of dest
// again (undoMove).
func (mbox *mailbox) MoveMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	if err := mbox.init(); err != nil {
		return err
	}

	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	dest := mbox.u.getMailbox(destName)
	if dest == nil {
		return imapbackend.ErrNoSuchMailbox
	}

	failed, err := mbox.u.c.LabelMessages(dest.label, apiIDs)
	if err != nil {
		return err
	}
	labelErr := mbox.u.applyAction(apiIDs, failed, labelUpdate(dest.label, true))
	labelled := succeeded(apiIDs, failed)
	if len(labelled) == 0 {
		return labelErr
	}

	var unlabelErr error
	failed, err = mbox.u.c.UnlabelMessages(mbox.label, labelled)
	if err != nil {
		unlabelErr = fmt.Errorf("cannot remove the messages from %v: %w", mbox.Name(), err)
		failed = make(map[string]error, len(labelled))
		for _, apiID := range labelled {
			failed[apiID] = err
		}
	} else {
		unlabelErr = mbox.u.applyAction(labelled, failed, labelUpdate(mbox.label, false))
	}
	inBoth := slices.DeleteFunc(slices.Clone(labelled), func(apiID string) bool {
		err, isFailed := failed[apiID]
		return !isFailed || nothingToDo(err)
	})
	if len(inBoth) == 0 {
		return labelErr
	}

	allMoved, err := mbox.undoMove(dest, inBoth)
	if err == nil && allMoved {
		// Proton had moved them all already: the unlabel failed for nothing.
		unlabelErr = nil
	}
	return errors.Join(labelErr, unlabelErr, err)
}

// succeeded returns the messages of apiIDs that an action's failures don't
// name, or name as nothing to do.
func succeeded(apiIDs []string, failed map[string]error) []string {
	var ok []string
	for _, apiID := range apiIDs {
		if err, isFailed := failed[apiID]; !isFailed || nothingToDo(err) {
			ok = append(ok, apiID)
		}
	}
	return ok
}

// undoMove settles the messages apiIDs, labelled with dest but not unlabelled
// from mbox, so that none stays in both. Labelling into a folder can have
// moved one out of mbox in Proton already: that one is moved, and only the
// local copy leaves mbox. One Proton still shows in mbox is unlabelled from
// dest, as if never moved. Never the other way round: unlabelling from dest a
// message mbox no longer holds would leave it in neither. allMoved reports
// whether every one turned out moved.
func (mbox *mailbox) undoMove(dest *mailbox, apiIDs []string) (allMoved bool, err error) {
	var moved, undo []string
	for chunk := range slices.Chunk(apiIDs, protonmail.MaxMessagesPerRequest) {
		msgs, err := mbox.u.c.MessagesByID(chunk)
		if err != nil {
			return false, fmt.Errorf("cannot check the messages left in both %v and %v until the next event: %w", mbox.Name(), dest.Name(), err)
		}
		for _, msg := range msgs {
			if slices.Contains(msg.LabelIDs, mbox.label) {
				undo = append(undo, msg.ID)
			} else {
				moved = append(moved, msg.ID)
			}
		}
	}

	var errs []error
	if len(moved) > 0 {
		errs = append(errs, mbox.u.applyAction(moved, nil, labelUpdate(mbox.label, false)))
	}
	if len(undo) > 0 {
		log.Printf("Taking %v messages out of %v again: they couldn't leave %v", len(undo), dest.Name(), mbox.Name())
		failed, err := mbox.u.c.UnlabelMessages(dest.label, undo)
		if err != nil {
			errs = append(errs, fmt.Errorf("cannot take the messages out of %v again, left in both until the next event: %w", dest.Name(), err))
		} else {
			errs = append(errs, mbox.u.applyAction(undo, failed, labelUpdate(dest.label, false)))
		}
	}
	return len(moved) == len(apiIDs), errors.Join(errs...)
}

// Expunge removes the messages marked \Deleted from this mailbox, as
// proton-bridge does (connector.go, removeMessagesFromMailboxWithUnlabelCall
// OnFolders): it unlabels them, so they leave this mailbox and stay in the
// others and in All Mail. Only from Trash and Drafts are they then deleted for
// good, and only those that no other mailbox or label still shows
// (expungeForGood). All Mail holds every message: nothing leaves it but by
// leaving every other mailbox, so EXPUNGE there is refused. The answer is
// that of the unlabel: deleting for good is peroxide's own step after it,
// whose failure is logged.
func (mbox *mailbox) Expunge() error {
	if mbox.label == protonmail.LabelAllMail {
		return errors.New("cannot expunge from All Mail: move the messages to Trash, or remove them from their mailboxes")
	}
	if err := mbox.init(); err != nil {
		return err
	}

	mbox.Lock()
	if len(mbox.deleted) == 0 {
		mbox.Unlock()
		return nil // Nothing to do
	}
	apiIDs := slices.Sorted(maps.Keys(mbox.deleted))
	mbox.Unlock()

	failed, err := mbox.u.c.UnlabelMessages(mbox.label, apiIDs)
	if err != nil {
		return err
	}

	// Removed messages are gone from here: don't remove them again on the
	// next EXPUNGE.
	var removed []string
	mbox.Lock()
	for _, apiID := range apiIDs {
		if err, ok := failed[apiID]; !ok || nothingToDo(err) {
			delete(mbox.deleted, apiID)
			removed = append(removed, apiID)
		}
	}
	mbox.Unlock()

	err = mbox.u.applyAction(apiIDs, failed, labelUpdate(mbox.label, false))
	if mbox.label == protonmail.LabelTrash || mbox.label == protonmail.LabelDraft {
		// The messages left the mailbox, as asked, and clients were told
		// (EXPUNGE): NO would contradict that. One not deleted for good stays
		// in All Mail, where the user can delete it again.
		if forGoodErr := mbox.expungeForGood(removed); forGoodErr != nil {
			log.Printf("Messages expunged from %v, not all deleted for good: %v", mbox.Name(), forGoodErr)
		}
	}
	return err
}

// expungeForGood deletes for good those of the messages apiIDs, just removed
// from Trash or Drafts, that no mailbox or label of the account shows any
// more. Labels Proton keeps for itself (All Mail, All Drafts, All Sent, All
// Scheduled, and those peroxide shows nothing for) don't keep a message, as
// in bridge; a label peroxide doesn't know yet is looked up (ListLabels) and
// keeps it if it is a folder or a label. A message these requests fail for is
// kept: it has left the mailbox, and stays in All Mail.
func (mbox *mailbox) expungeForGood(apiIDs []string) error {
	var forGood []string
	var listed map[string]*protonmail.Label // labels from Proton, fetched once if needed
	for chunk := range slices.Chunk(apiIDs, protonmail.MaxMessagesPerRequest) {
		msgs, err := mbox.u.c.MessagesByID(chunk)
		if err != nil {
			return fmt.Errorf("cannot check what else holds the messages removed from %v, kept in All Mail: %w", mbox.Name(), err)
		}
		for _, msg := range msgs {
			kept, err := mbox.u.keepsMessage(msg, mbox.label, &listed)
			if err != nil {
				return fmt.Errorf("cannot check what else holds the messages removed from %v, kept in All Mail: %w", mbox.Name(), err)
			}
			if kept == "" {
				forGood = append(forGood, msg.ID)
			} else {
				log.Printf("Message %v left %v, not deleted: label %v still holds it", msg.ID, mbox.Name(), kept)
			}
		}
	}
	if len(forGood) == 0 {
		return nil
	}

	log.Printf("Deleting %v messages for good, expunged from %v", len(forGood), mbox.Name())
	failed, err := mbox.u.c.DeleteMessages(forGood)
	if err != nil {
		return fmt.Errorf("cannot delete the messages expunged from %v, kept in All Mail: %w", mbox.Name(), err)
	}
	return mbox.u.applyDeleteAction(forGood, failed)
}

// keepsMessage returns a label of msg, other than from, for which the message
// must not be deleted for good, or "" if there is none. listed caches the
// account's labels once fetched.
func (u *user) keepsMessage(msg *protonmail.Message, from string, listed *map[string]*protonmail.Label) (string, error) {
	for _, labelID := range msg.LabelIDs {
		switch labelID {
		case from, protonmail.LabelAllMail, protonmail.LabelAllDraft, protonmail.LabelAllSent, protonmail.LabelAllScheduled:
			continue
		}
		if u.showsLabel(labelID) {
			return labelID, nil
		}
		if *listed == nil {
			labels, err := u.c.ListLabels()
			if err != nil {
				return "", fmt.Errorf("cannot list labels: %w", err)
			}
			*listed = make(map[string]*protonmail.Label, len(labels))
			for _, label := range labels {
				(*listed)[label.ID] = label
			}
		}
		// GET /labels lists folders and labels, not Proton's own: a label
		// missing from it is one of those, which keeps nothing.
		if label, ok := (*listed)[labelID]; ok && label.Type == protonmail.LabelMessage {
			return labelID, nil
		}
	}
	return "", nil
}

// showsLabel reports whether labelID is a mailbox or a flag of the account.
func (u *user) showsLabel(labelID string) bool {
	u.Lock()
	defer u.Unlock()
	_, isMailbox := u.mailboxes[labelID]
	_, isFlag := u.flags[labelID]
	return isMailbox || isFlag
}

func (mbox *mailbox) Poll() error {
	mbox.u.poll()
	return nil
}
