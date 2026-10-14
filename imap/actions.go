package imap

import (
	"errors"
	"fmt"
	"log"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/protonmail"
)

// applyUpdate applies update to the stored message apiID, and returns the
// updates telling clients about it.
func (u *user) applyUpdate(apiID string, update *protonmail.EventMessageUpdate) ([]imapbackend.Update, error) {
	createdSeqNums, deletedSeqNums, err := u.db.UpdateMessage(apiID, update)
	if err != nil {
		return nil, fmt.Errorf("cannot update message in local DB: %w", err)
	}

	var updates []imapbackend.Update
	for labelID, seqNum := range createdSeqNums {
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.MailboxUpdate)
			up.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
			up.MailboxStatus = imap.NewMailboxStatus(mbox.name, []imap.StatusItem{imap.StatusMessages})
			up.MailboxStatus.Messages = seqNum
			updates = append(updates, up)
		}
	}
	for labelID, seqNum := range deletedSeqNums {
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.ExpungeUpdate)
			up.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
			up.SeqNum = seqNum
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
			log.Printf("cannot get sequence number of message %v in %v: %v", apiID, mbox.name, err)
			continue
		}

		up := new(imapbackend.MessageUpdate)
		up.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
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
	u.messages.remove(apiID)

	seqNums, err := u.db.DeleteMessage(apiID)
	if err != nil {
		return nil, fmt.Errorf("cannot delete message from local DB: %w", err)
	}

	var updates []imapbackend.Update
	for labelID, seqNum := range seqNums {
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.ExpungeUpdate)
			up.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
			up.SeqNum = seqNum
			updates = append(updates, up)
		}
	}
	return updates, nil
}

// sendUpdates hands updates to the IMAP server, and waits until they were
// written to every client concerned.
func (u *user) sendUpdates(updates []imapbackend.Update) {
	for _, update := range updates {
		// go-imap creates Done's channel lazily without synchronization, and
		// it's waited on below: create it first.
		update.Done()
		u.backend.updates <- update
	}
	for _, update := range updates {
		<-update.Done()
	}
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
	u.sendUpdates(updates)
	u.pollSoon()
	return actionError(apiIDs, failed)
}

// applyDeleteAction is applyAction for deleting messages.
func (u *user) applyDeleteAction(apiIDs []string, failed map[string]error) error {
	var updates []imapbackend.Update
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
	u.sendUpdates(updates)
	u.pollSoon()
	return actionError(apiIDs, failed)
}

// pollSoon asks for Proton's events without waiting for them. They confirm
// what an action did locally, and tell what else it did: e.g. labelling a
// message with an exclusive folder removes it from the others.
func (u *user) pollSoon() {
	if u.eventsReceiver != nil {
		go u.eventsReceiver.Poll()
	}
}

func unreadUpdate(unread int) *protonmail.EventMessageUpdate {
	return &protonmail.EventMessageUpdate{Unread: &unread}
}

func labelUpdate(labelID string, add bool) *protonmail.EventMessageUpdate {
	if add {
		return &protonmail.EventMessageUpdate{LabelIDsAdded: []string{labelID}, LabelIDsRemoved: []string{}}
	}
	return &protonmail.EventMessageUpdate{LabelIDsAdded: []string{}, LabelIDsRemoved: []string{labelID}}
}
