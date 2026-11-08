package imap

import (
	"context"
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

	sync.Mutex // protects everything below

	deleted map[string]struct{}
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

	// From the local database, kept by the download and by events: while the
	// download runs, the counts grow with it, as proton-bridge's do during its
	// sync. One transaction, so that no change falls between the fields.
	s, err := mbox.db.Status()
	if err != nil {
		return nil, fmt.Errorf("cannot read the status of mailbox %v: %w", mbox.Name(), err)
	}

	for _, name := range items {
		switch name {
		case imap.StatusMessages:
			status.Messages = s.Total
		case imap.StatusUidNext:
			status.UidNext = s.UidNext
		case imap.StatusUidValidity:
			status.UidValidity = s.UidValidity
		case imap.StatusRecent:
			status.Recent = 0
		case imap.StatusUnseen:
			status.Unseen = s.Unread
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

// delete forgets the mailbox after its folder was deleted. Clients that still
// have it selected get errors from then on. A write that grows the database
// file waits for open read transactions, so no reader may wait for a lock
// inside one (SearchMessages and flagsUpdates collect entries first). The
// bucket is deleted without mbox's lock, as nothing needs it held. Called
// while an event page holds user.syncSem, so no download page refills the
// bucket meanwhile.
func (mbox *mailbox) delete() error {
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

// fetchMessage fetches items of the message apiID, which has seqNum and uid
// in the mailbox. FLAGS, INTERNALDATE and UID come from the metadata; every
// other item from the message as stored, which is built and stored first if
// it isn't, so that what is served never changes under a UID.
func (mbox *mailbox) fetchMessage(seqNum, uid uint32, apiID string, items []imap.FetchItem) (*imap.Message, *protonmail.Message, error) {
	sections, err := parseSections(items)
	if err != nil {
		return nil, nil, err
	}
	msg, err := mbox.u.db.Message(apiID)
	if err != nil {
		return nil, nil, err
	}

	var rec *database.BodyRecord
	var literal []byte
	if needsContent(items) {
		// go-imap v1 gives a backend no context: the request runs to its end.
		if rec, literal, err = mbox.u.storedMessage(context.Background(), apiID); err != nil {
			return nil, nil, err
		}
	}

	fetched := imap.NewMessage(seqNum, items)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			fetched.Envelope = rec.Envelope
		case imap.FetchBodyStructure:
			fetched.BodyStructure = rec.BodyStructure
		case imap.FetchBody:
			fetched.BodyStructure = withoutExtension(rec.BodyStructure)
		case imap.FetchFlags:
			fetched.Flags = mbox.fetchFlags(msg)
		case imap.FetchInternalDate:
			fetched.InternalDate = msg.Time.Time()
		case imap.FetchRFC822Size:
			fetched.Size = rec.Size
		case imap.FetchUid:
			fetched.Uid = uid
		case imap.FetchAll, imap.FetchFast, imap.FetchFull:
			panic("invariant: ListMessages expands FETCH macros")
		case imap.FetchRFC822, imap.FetchRFC822Header, imap.FetchRFC822Text:
			// The old names of BODY[], BODY.PEEK[HEADER] and BODY[TEXT].
			if err := addSection(fetched, sections[item], literal, rec, apiID); err != nil {
				return nil, nil, err
			}
		default:
			// BODY[...] and BODY.PEEK[...], which name any section.
			if err := addSection(fetched, sections[item], literal, rec, apiID); err != nil {
				return nil, nil, err
			}
		}
	}

	return fetched, msg, nil
}

// addSection adds section of the stored message literal, of record rec, to
// fetched.
func addSection(fetched *imap.Message, section *imap.BodySectionName, literal []byte, rec *database.BodyRecord, apiID string) error {
	l, err := bodySection(literal, rec.BodyStructure, section)
	if err != nil {
		return fmt.Errorf("cannot fetch %v of message %v: %w", section.FetchItem(), apiID, err)
	}
	fetched.Body[section] = l
	return nil
}

// parseSections returns the body sections items name, by item. An item that
// is neither a known one nor a section is an error: answering without it
// would drop it silently.
func parseSections(items []imap.FetchItem) (map[imap.FetchItem]*imap.BodySectionName, error) {
	sections := make(map[imap.FetchItem]*imap.BodySectionName)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope, imap.FetchBody, imap.FetchBodyStructure, imap.FetchFlags,
			imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchUid,
			imap.FetchAll, imap.FetchFast, imap.FetchFull:
			continue
		case imap.FetchRFC822, imap.FetchRFC822Header, imap.FetchRFC822Text:
			// Sections under their old names, which ParseBodySectionName
			// reads as the sections they name.
		default:
			// BODY[...] and BODY.PEEK[...], or an item go-imap doesn't
			// know.
		}
		section, err := imap.ParseBodySectionName(item)
		if err != nil {
			return nil, fmt.Errorf("unknown FETCH item %v: %w", item, err)
		}
		sections[item] = section
	}
	return sections, nil
}

// needsContent reports whether fetching items reads the message's contents,
// not only its metadata.
func needsContent(items []imap.FetchItem) bool {
	for _, item := range items {
		switch item {
		case imap.FetchFlags, imap.FetchInternalDate, imap.FetchUid:
		case imap.FetchEnvelope, imap.FetchBody, imap.FetchBodyStructure, imap.FetchRFC822Size,
			imap.FetchRFC822, imap.FetchRFC822Header, imap.FetchRFC822Text:
			return true
		case imap.FetchAll, imap.FetchFast, imap.FetchFull:
			panic("invariant: ListMessages expands FETCH macros")
		default:
			// BODY[...] and BODY.PEEK[...].
			return true
		}
	}
	return false
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

	// Every message is fetched with the same items, so an unknown one fails
	// the FETCH before any message is built or sent.
	items = expandFetchItems(items)
	if _, err := parseSections(items); err != nil {
		return nil, err
	}
	opens := setsSeen(items)

	// The entries in one pass over the mailbox, in one read transaction, and
	// the messages read after it ends (as in SearchMessages: a nested read
	// transaction deadlocks with writers). Looking each up by number walked
	// the mailbox twice per message.
	entries, err := mbox.entries(uid, seqSet)
	if err != nil {
		return nil, err
	}
	content := needsContent(items)
	for i, e := range entries {
		if content && i%fetchChunk == 0 {
			// The next chunk's unstored messages, built at once.
			mbox.u.prefetch(entries[i:min(i+fetchChunk, len(entries))])
		}
		fetched, msg, err := mbox.fetchMessage(e.seqNum, e.uid, e.apiID, items)
		if errors.Is(err, errMessageGone) {
			// Its delete event removes it, with an EXPUNGE, at the next
			// poll: failing the FETCH would also drop the messages after it.
			log.Printf("Message %v of %v skipped: %v", e.seqNum, mbox.Name(), err)
			continue
		} else if errors.Is(err, database.ErrNotFound) {
			continue // deleted by an event since the entries were read
		} else if err != nil {
			return nil, err
		}
		ch <- fetched
		if opens && msg.Unread == 1 {
			opened = append(opened, msg.ID)
		}
	}

	return opened, nil
}

// fetchChunk is how many messages of a FETCH are built at once before they
// are sent: twice the most requests the account may make at once, so that
// the requests stay busy, and the response starts after one chunk rather
// than after the whole FETCH.
const fetchChunk = 2 * maxParallel

// prefetch builds and stores the messages of entries that aren't stored
// yet, in parallel, paced by the account's limiter as the background
// download is. Each would otherwise cost the FETCH a round trip in turn.
// Each is tried once: a message that fails (throttled included, which also
// slows the download) is left to fetchMessage, which builds it again and
// reports why, as a FETCH always did; one already being built (by the
// download) is waited for there too.
func (u *user) prefetch(entries []mailboxEntry) {
	ctx := context.Background() // go-imap v1 gives a backend no context
	limiter := u.pacer()
	var wg sync.WaitGroup
	for _, e := range entries {
		if _, err := u.db.BodyRecord(e.apiID); !errors.Is(err, database.ErrNotFound) {
			continue // stored, or an error fetchMessage reports
		}
		if failed, err := u.db.Failed(e.apiID); err != nil || failed {
			continue // left to fetchMessage: one try per FETCH, as before
		}
		if err := limiter.acquire(ctx); err != nil {
			panic("invariant: acquire fails only when its context ends")
		}
		wg.Go(func() {
			_, _, err := u.storedMessage(ctx, e.apiID)
			var after time.Duration
			if apiErr, ok := errors.AsType[*protonmail.APIError](err); ok {
				after = apiErr.RetryAfter
			}
			limiter.release(err != nil && retryable(err), after)
		})
	}
	wg.Wait()
}

// mailboxEntry is a message of a mailbox with its numbers there.
type mailboxEntry struct {
	seqNum, uid uint32
	apiID       string
}

// entries returns the messages of seqSet, by UID if isUID, else by sequence
// number, in ascending order. "*" is the last message (RFC 3501 section 9,
// seq-number), so "n:*" holds it even if n is past it.
func (mbox *mailbox) entries(isUID bool, seqSet *imap.SeqSet) ([]mailboxEntry, error) {
	var all []mailboxEntry
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		all = append(all, mailboxEntry{seqNum, uid, apiID})
		return nil
	})
	if err != nil || len(all) == 0 {
		return nil, err
	}
	last := all[len(all)-1].seqNum
	if isUID {
		last = all[len(all)-1].uid
	}
	var set imap.SeqSet
	for _, seq := range seqSet.Set {
		start, stop := seq.Start, seq.Stop
		if start == 0 {
			start = last
		}
		if stop == 0 {
			stop = last
		}
		set.AddRange(min(start, stop), max(start, stop))
	}
	var picked []mailboxEntry
	for _, e := range all {
		id := e.seqNum
		if isUID {
			id = e.uid
		}
		if set.Contains(id) {
			picked = append(picked, e)
		}
	}
	return picked, nil
}

func (mbox *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if body.Len() > appendLimit {
		return imapbackend.ErrTooBig
	}
	if mbox.label != protonmail.LabelDraft {
		return errors.New("cannot create messages outside the Drafts mailbox")
	}

	_, err := createMessage(mbox.u.c, mbox.u.u, mbox.u.privateKeys, mbox.u.addresses(), body)
	if err != nil {
		return err
	}

	return mbox.Poll()
}

func (mbox *mailbox) fromSeqSet(isUID bool, seqSet *imap.SeqSet) ([]string, error) {
	entries, err := mbox.entries(isUID, seqSet)
	apiIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		apiIDs = append(apiIDs, e.apiID)
	}
	return apiIDs, err
}

func (mbox *mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, op imap.FlagsOp, flags []string) error {
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
		// that sent .SILENT. Under syncSem, so that its FETCH reaches clients
		// in turn with the sequence numbers it was made with.
		mbox.u.lockSync()
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
		mbox.u.unlockSyncAndSend(updates, mbox.u.sendUpdates)
		return err
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

// MoveMessages moves the messages to dest as proton-bridge does by default
// (connector.go, moveMessagesWithUnlabelCallOnFolders): label with dest, then
// unlabel from mbox. Labelling into a folder already takes a message out of
// the folder it was in, and bridge once dropped the unlabel for that
// (BRIDGE-451), but put it back after clients broke (BRIDGE-522); a message
// the unlabel finds already gone answers "does not exist", which counts as
// done. Each message must end in one of the two, and should not end in both,
// even when the command fails (RFC 6851 section 3.3): one dest's label failed
// for is left alone, and one the unlabel failed for is checked and taken out
// of dest again (undoMove).
func (mbox *mailbox) MoveMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	apiIDs, err := mbox.fromSeqSet(uid, seqSet)
	if err != nil {
		return err
	}

	dest := mbox.u.getMailbox(destName)
	if dest == nil {
		return imapbackend.ErrNoSuchMailbox
	}
	if dest.label == mbox.label {
		// Labelling would do nothing, and unlabelling would take the
		// messages out of the mailbox they are to stay in.
		return nil
	}
	if err := moveAllowed(mbox.label, dest.label); err != nil {
		return err
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

// moveAllowed refuses, before any request, moves into or out of a mailbox
// that is a view of messages kept in their folders, and those between INBOX
// and Sent. All Mail and INBOX/Sent proton-bridge refuses too
// (moveMessagesWithUnlabelCallOnFolders). Starred is the same kind of view as
// All Mail, though bridge lets it be moved into and out of: a move into it
// would unlabel the message from its folder, leaving it in All Mail only, and
// one out of it would file it in the destination for what is, in Proton, an
// unstar. Starring is the \Flagged flag, or COPY to Starred.
func moveAllowed(from, to string) error {
	switch {
	case from == protonmail.LabelAllMail:
		return errors.New("cannot move out of All Mail: move the message from its own mailbox")
	case to == protonmail.LabelAllMail:
		return errors.New("cannot move into All Mail: every message is there already")
	case from == protonmail.LabelStarred:
		return errors.New(`cannot move out of Starred: unstar with STORE -FLAGS (\Flagged), or move the message from its own mailbox`)
	case to == protonmail.LabelStarred:
		return errors.New(`cannot move into Starred: star with STORE +FLAGS (\Flagged), or COPY to Starred`)
	case from == protonmail.LabelInbox && to == protonmail.LabelSent,
		from == protonmail.LabelSent && to == protonmail.LabelInbox:
		return errors.New("cannot move between INBOX and Sent")
	}
	return nil
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
// from mbox, so that none stays in both. Proton can have taken one out of mbox
// already: that one is moved, and only the local copy leaves mbox. One Proton
// still shows in mbox is unlabelled from dest, as if never moved. Never the
// other way round: unlabelling from dest a message mbox no longer holds would
// leave it in neither. allMoved reports whether every one turned out moved.
func (mbox *mailbox) undoMove(dest *mailbox, apiIDs []string) (allMoved bool, err error) {
	var moved, undo []string
	for chunk := range slices.Chunk(apiIDs, protonmail.MaxMessagesPerRequest) {
		msgs, err := mbox.u.c.MessagesByID(context.Background(), chunk)
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
			errs = append(errs, fmt.Errorf("cannot take the messages out of %v again: left in both %v and %v, in Proton too: %w", dest.Name(), mbox.Name(), dest.Name(), err))
		} else {
			errs = append(errs, mbox.u.applyAction(undo, failed, labelUpdate(dest.label, false)))
		}
	}
	return len(moved) == len(apiIDs), errors.Join(errs...)
}

// Expunge removes the messages marked \Deleted from this mailbox. From Trash
// and Drafts, those no other mailbox or label shows are deleted for good
// (forGood), as proton-bridge does (connector.go,
// removeMessagesFromMailboxWithUnlabelCallOnFolders); the others are
// unlabelled, so they leave this mailbox and stay in the others and in All
// Mail. Unlike bridge, the delete comes first and the unlabel only after, for
// the messages not deleted: one the delete fails for is left as it was, in
// this mailbox and marked \Deleted, so that NO is the truth and a second
// EXPUNGE tries again. Unlabelled first, it would have left for All Mail
// only, which clients may not show. All Mail holds every message: nothing
// leaves it but by leaving every other mailbox, so EXPUNGE there is refused.
func (mbox *mailbox) Expunge() error {
	if mbox.label == protonmail.LabelAllMail {
		return errors.New("cannot expunge from All Mail: move the messages to Trash, or remove them from their mailboxes")
	}

	mbox.Lock()
	if len(mbox.deleted) == 0 {
		mbox.Unlock()
		return nil // Nothing to do
	}
	apiIDs := slices.Sorted(maps.Keys(mbox.deleted))
	mbox.Unlock()

	unlabel := apiIDs
	var deleteErr error
	if mbox.label == protonmail.LabelTrash || mbox.label == protonmail.LabelDraft {
		forGood, err := mbox.forGood(apiIDs)
		if err != nil {
			return err
		}
		if len(forGood) > 0 {
			log.Printf("Deleting %v messages for good, expunged from %v", len(forGood), mbox.Name())
			failed, err := mbox.u.c.DeleteMessages(forGood)
			if err != nil {
				return fmt.Errorf("cannot delete the messages, none expunged from %v: %w", mbox.Name(), err)
			}
			mbox.forgetDeleted(succeeded(forGood, failed))
			deleteErr = mbox.u.applyDeleteAction(forGood, failed)

			tried := make(map[string]struct{}, len(forGood))
			for _, apiID := range forGood {
				tried[apiID] = struct{}{}
			}
			unlabel = slices.DeleteFunc(slices.Clone(apiIDs), func(apiID string) bool {
				_, ok := tried[apiID]
				return ok
			})
		}
	}
	if len(unlabel) == 0 {
		return deleteErr
	}

	failed, err := mbox.u.c.UnlabelMessages(mbox.label, unlabel)
	if err != nil {
		return errors.Join(deleteErr, fmt.Errorf("cannot remove the messages from %v: %w", mbox.Name(), err))
	}
	mbox.forgetDeleted(succeeded(unlabel, failed))
	return errors.Join(deleteErr, mbox.u.applyAction(unlabel, failed, labelUpdate(mbox.label, false)))
}

// forgetDeleted unmarks apiIDs, gone from the mailbox: the next EXPUNGE
// mustn't remove them again.
func (mbox *mailbox) forgetDeleted(apiIDs []string) {
	mbox.Lock()
	defer mbox.Unlock()
	for _, apiID := range apiIDs {
		delete(mbox.deleted, apiID)
	}
}

// forGood returns those of the messages apiIDs, marked \Deleted in Trash or
// Drafts, that no other mailbox or label of the account shows: expunged, they
// are deleted for good. Labels Proton keeps for itself (All Mail, All Drafts,
// All Sent, All Scheduled, and those peroxide shows nothing for) don't keep a
// message, as in bridge; a label peroxide doesn't know yet is looked up
// (ListLabels) and keeps it if it is a folder or a label. A message Proton no
// longer has is left out, and unlabelled as the others, which does nothing.
func (mbox *mailbox) forGood(apiIDs []string) ([]string, error) {
	var forGood []string
	var listed map[string]*protonmail.Label // labels from Proton, fetched once if needed
	for chunk := range slices.Chunk(apiIDs, protonmail.MaxMessagesPerRequest) {
		msgs, err := mbox.u.c.MessagesByID(context.Background(), chunk)
		if err != nil {
			return nil, fmt.Errorf("cannot check what else holds the messages, none expunged from %v: %w", mbox.Name(), err)
		}
		for _, msg := range msgs {
			kept, err := mbox.u.keepsMessage(msg, mbox.label, &listed)
			if err != nil {
				return nil, fmt.Errorf("cannot check what else holds the messages, none expunged from %v: %w", mbox.Name(), err)
			}
			if kept == "" {
				forGood = append(forGood, msg.ID)
			} else {
				log.Printf("Message %v leaves %v, not deleted: label %v still holds it", msg.ID, mbox.Name(), kept)
			}
		}
	}
	return forGood, nil
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
