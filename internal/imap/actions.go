package imap

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// mailboxUpdate is the target of an update for mbox. go-imap matches it to
// the clients that have the mailbox selected by name, when it delivers it
// (Server.listenUpdates), so the name is read then: an update made just
// before the folder is renamed in Proton would otherwise reach no client,
// and its EXISTS or EXPUNGE be lost.
type mailboxUpdate struct {
	username string
	mbox     *mailbox
	done     chan struct{}
}

var _ imapbackend.Update = (*mailboxUpdate)(nil) // proof: compile-time

func newMailboxUpdate(username string, mbox *mailbox) *mailboxUpdate {
	return &mailboxUpdate{username: username, mbox: mbox, done: make(chan struct{})}
}

func (up *mailboxUpdate) Username() string { return up.username }

func (up *mailboxUpdate) Mailbox() string { return up.mbox.Name() }

func (up *mailboxUpdate) Done() chan struct{} { return up.done }

// applyUpdate applies update to the stored message apiID, and returns the
// updates telling clients about it.
func (u *user) applyUpdate(apiID string, update *protonmail.EventMessageUpdate) ([]imapbackend.Update, error) {
	createdSeqNums, deletedSeqNums, err := u.db.UpdateMessage(apiID, update)
	if err != nil {
		return nil, fmt.Errorf("cannot update message in local DB: %w", err)
	}

	var updates []imapbackend.Update
	// Label ID order, not map order: logs and tests see the same each time.
	for _, labelID := range slices.Sorted(maps.Keys(createdSeqNums)) {
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.MailboxUpdate)
			up.Update = newMailboxUpdate(u.u.Name, mbox)
			up.MailboxStatus = imap.NewMailboxStatus(mbox.Name(), []imap.StatusItem{imap.StatusMessages})
			up.MailboxStatus.Messages = createdSeqNums[labelID]
			updates = append(updates, up)
		}
	}
	for _, labelID := range slices.Sorted(maps.Keys(deletedSeqNums)) {
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.ExpungeUpdate)
			up.Update = newMailboxUpdate(u.u.Name, mbox)
			up.SeqNum = deletedSeqNums[labelID]
			updates = append(updates, up)
		}
	}

	msg, err := u.db.Message(apiID)
	if err != nil {
		return updates, fmt.Errorf("cannot get updated message from local DB: %w", err)
	}
	for _, labelID := range msg.LabelIDs {
		if _, created := createdSeqNums[labelID]; created {
			// Clients learn about it from the new message count.
			continue
		}
		mbox := u.getMailboxByLabel(labelID)
		if mbox == nil {
			continue
		}
		seqNum, uid, err := mbox.db.FromApiID(apiID)
		if err != nil {
			log.Printf("cannot get sequence number of message %v in %v: %v", apiID, mbox.Name(), err)
			continue
		}

		up := new(imapbackend.MessageUpdate)
		up.Update = newMailboxUpdate(u.u.Name, mbox)
		// Clients may have asked for this with a UID command, and then only
		// take responses bearing the UID (RFC 3501 section 6.4.8).
		up.Message = imap.NewMessage(seqNum, []imap.FetchItem{imap.FetchFlags, imap.FetchUid})
		up.Message.Flags = mbox.fetchFlags(msg)
		up.Message.Uid = uid
		updates = append(updates, up)
	}
	return updates, nil
}

// applyDelete deletes the stored message apiID, and returns the updates
// telling clients about it.
func (u *user) applyDelete(apiID string) ([]imapbackend.Update, error) {
	seqNums, err := u.db.DeleteMessage(apiID)
	if err != nil {
		return nil, fmt.Errorf("cannot delete message from local DB: %w", err)
	}

	var updates []imapbackend.Update
	for _, labelID := range slices.Sorted(maps.Keys(seqNums)) {
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.ExpungeUpdate)
			up.Update = newMailboxUpdate(u.u.Name, mbox)
			up.SeqNum = seqNums[labelID]
			updates = append(updates, up)
		}
	}
	return updates, nil
}

// sendUpdates hands updates to the IMAP server, and waits until they were
// written to every client concerned, one after the other. Called with
// sendSem (unlockSyncAndSend).
func (u *user) sendUpdates(updates []imapbackend.Update) {
	u.sendOn(u.backend.updates, updates)
}

// nothingToDo reports whether err, the result of an action on a message, means
// the message already was as asked. Proton calls it "does not exist", which is
// also the result for missing messages: those aren't stored, so the action
// isn't asked for them.
func nothingToDo(err error) bool {
	var apiErr *protonmail.APIError
	return errors.As(err, &apiErr) && apiErr.Code == protonmail.CodeMessageDoesNotExist
}

// actionError returns an error for the messages of apiIDs an action failed for.
func actionError(apiIDs []string, failed map[string]error) error {
	var errs []error
	for _, apiID := range apiIDs {
		if err, ok := failed[apiID]; ok && !nothingToDo(err) {
			errs = append(errs, fmt.Errorf("message %v: %w", apiID, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("failed for %v of %v messages: %w", len(errs), len(apiIDs), errors.Join(errs...))
}

// applyAction applies update to the messages of apiIDs an action of
// peroxide's succeeded for, and tells clients, without waiting for Proton's
// event about it. failed holds the error of each message the action failed
// for, as returned by the API.
func (u *user) applyAction(apiIDs []string, failed map[string]error, update *protonmail.EventMessageUpdate) error {
	var updates []imapbackend.Update
	// A COPY or MOVE adds the messages to the destination: not while it syncs
	// (user.syncSem). Sent after, as event pages are.
	u.lockSync()
	for _, apiID := range apiIDs {
		if err, ok := failed[apiID]; ok && !nothingToDo(err) {
			continue
		}
		up, err := u.applyUpdate(apiID, update)
		updates = append(updates, up...)
		if err != nil {
			log.Printf("cannot apply action to message %v: %v", apiID, err)
		}
	}
	u.unlockSyncAndSend(updates, u.sendUpdates)
	u.pollSoon()
	return actionError(apiIDs, failed)
}

// applyDeleteAction is applyAction for deleting messages.
func (u *user) applyDeleteAction(apiIDs []string, failed map[string]error) error {
	var updates []imapbackend.Update
	u.lockSync()
	for _, apiID := range apiIDs {
		if err, ok := failed[apiID]; ok && !nothingToDo(err) {
			continue
		}
		up, err := u.applyDelete(apiID)
		updates = append(updates, up...)
		if err != nil {
			log.Printf("cannot apply deletion of message %v: %v", apiID, err)
		}
	}
	u.unlockSyncAndSend(updates, u.sendUpdates)
	u.pollSoon()
	return actionError(apiIDs, failed)
}

// pollSoon asks for Proton's events without waiting for them. They confirm
// what an action did locally, and tell what else it did: e.g. labelling a
// message with an exclusive folder removes it from the others.
func (u *user) pollSoon() {
	if u.eventsReceiver != nil {
		u.eventsReceiver.Poll()
	}
}

func unreadUpdate(unread int) *protonmail.EventMessageUpdate {
	return &protonmail.EventMessageUpdate{Unread: &unread}
}

// labelUpdate is the local change of adding or removing labelID. Proton takes
// Starred off a message it files in Trash (seen in its events), so adding
// Trash removes Starred too: clients don't show \Flagged until the event.
func labelUpdate(labelID string, add bool) *protonmail.EventMessageUpdate {
	if add && labelID == protonmail.LabelTrash {
		return &protonmail.EventMessageUpdate{LabelIDsAdded: []string{labelID}, LabelIDsRemoved: []string{protonmail.LabelStarred}}
	}
	if add {
		return &protonmail.EventMessageUpdate{LabelIDsAdded: []string{labelID}, LabelIDsRemoved: []string{}}
	}
	return &protonmail.EventMessageUpdate{LabelIDsAdded: []string{}, LabelIDsRemoved: []string{labelID}}
}
