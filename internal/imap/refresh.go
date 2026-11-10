package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/internal/imap/database"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// refreshMessage builds again the stored message apiID, whose contents may
// have changed (an edited draft, a key unlocked since), as gluon does
// (applyMessageUpdated): if the bytes built differ from those stored, the
// message gets new UIDs with the new bytes, and the updates telling clients
// (EXPUNGE of the old, EXISTS of the new) are delivered in turn; if they are
// the same, nothing changes. A message not stored keeps its UIDs: its next
// FETCH builds what Proton has then, and a build of it running now isn't
// stored.
//
// Called without syncSem, by the events goroutine after the page that
// changed the message: the request, bounded by unknownMessageTimeout, holds
// no lock, so no event or download page waits on the network. syncSem is
// taken only to compare and renew. A later event that drops the message
// (another edit) makes this build stale, and a delete removes it: either way
// it isn't stored.
//
// A failure that retrying soon may fix (retryable) leaves what is stored, as
// clients have it; the next update of the message, or the next login for a
// key change, tries again. Another drops what is stored, so the next FETCH
// builds what Proton has, and puts the message in the failed set: what is
// served may then change under a UID, which beats serving an old draft for
// good. The error is returned for the log.
func (u *user) refreshMessage(apiID string, deliver func([]imapbackend.Update)) error {
	// Owning the build keeps FETCHes from building it meanwhile.
	bd, owner := u.builds.start(apiID)
	if !owner {
		// Being built, so not stored: what the build downloaded may be
		// older than this event.
		return u.builds.drop(u.db, apiID)
	}
	defer u.builds.finish(apiID)

	_, old, err := u.db.Body(apiID)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), unknownMessageTimeout)
	defer cancel()
	built, err := u.downloadAndBuild(ctx, apiID)
	if errors.Is(err, errMessageGone) {
		return nil // its delete event follows
	} else if protonmail.Retryable(err) {
		return err
	} else if err != nil {
		log.Printf("Message %v changed and can't be built again: dropped until a FETCH tries again: %v", apiID, err)
		if dropErr := u.builds.drop(u.db, apiID); dropErr != nil {
			return errors.Join(err, dropErr)
		}
		return u.recordFailure(apiID, err)
	}
	if bytes.Equal(built.literal, old) {
		return nil
	}

	u.lockSync()
	if u.builds.isStale(bd) {
		// Dropped by a later event, which refreshes it in turn.
		u.unlockSync()
		return nil
	}
	log.Printf("Message %v changed: it gets new UIDs", apiID)
	r, err := u.db.RenewMessage(apiID, built.literal, built.info)
	if err != nil {
		u.unlockSync()
		if errors.Is(err, database.ErrNotFound) {
			return nil // deleted meanwhile
		}
		return fmt.Errorf("cannot give message %v new UIDs: %w", apiID, err)
	}
	u.unlockSyncAndSend(u.renewalUpdates(r), deliver)
	return nil
}

// renewalUpdates tells clients of a message given new UIDs: in each mailbox,
// the EXPUNGE of its old sequence number, then its new count (EXISTS).
func (u *user) renewalUpdates(r database.Renewal) []imapbackend.Update {
	var updates []imapbackend.Update
	for _, labelID := range slices.Sorted(maps.Keys(r.Expunged)) {
		mbox := u.getMailboxByLabel(labelID)
		if mbox == nil {
			continue
		}
		expunge := new(imapbackend.ExpungeUpdate)
		expunge.Update = newMailboxUpdate(u.u.Name, mbox)
		expunge.SeqNum = r.Expunged[labelID]
		updates = append(updates, expunge)

		exists := new(imapbackend.MailboxUpdate)
		exists.Update = newMailboxUpdate(u.u.Name, mbox)
		exists.MailboxStatus = imap.NewMailboxStatus(mbox.Name(), []imap.StatusItem{imap.StatusMessages})
		exists.MailboxStatus.Messages = r.Created[labelID]
		updates = append(updates, exists)
	}
	return updates
}

// downloadAndBuild downloads and builds the message apiID. A message Proton
// no longer has is errMessageGone. It holds its share of the account's
// memory budget (u.mem), by Proton's size of the message, while downloading
// and building: a FETCH waits, as the background download does, until the
// builds running leave it room. The built bytes outlive the budget, held by
// the caller until stored or written.
func (u *user) downloadAndBuild(ctx context.Context, apiID string) (builtMessage, error) {
	var size int64
	msg, err := u.db.Message(apiID)
	if err == nil {
		size = msg.Size
	} else if !errors.Is(err, database.ErrNotFound) {
		return builtMessage{}, fmt.Errorf("cannot read the size of message %v: %w", apiID, err)
	}
	// Deleted meanwhile: built all the same, at no cost to the budget.
	if err := u.mem.acquire(ctx, size); err != nil {
		return builtMessage{}, err
	}
	defer u.mem.release(size)

	full, err := u.c.GetMessage(ctx, apiID)
	if gone(err) {
		return builtMessage{}, fmt.Errorf("%w: %v", errMessageGone, err)
	} else if err != nil {
		return builtMessage{}, err
	}
	built, err := u.buildMessage(ctx, full)
	if gone(err) {
		// An attachment gone with its message.
		return builtMessage{}, fmt.Errorf("%w: %v", errMessageGone, err)
	}
	return built, err
}

// unlockedKeysDigest names the keys u has unlocked: SHA-256 of their
// fingerprints, subkeys included (they decrypt), sorted.
func (u *user) unlockedKeysDigest() []byte {
	var fps [][]byte
	for _, e := range u.privateKeys {
		fps = append(fps, e.PrimaryKey.Fingerprint)
		for _, sub := range e.Subkeys {
			fps = append(fps, sub.PublicKey.Fingerprint)
		}
	}
	slices.SortFunc(fps, bytes.Compare)
	h := sha256.New()
	for _, fp := range fps {
		h.Write(fp)
	}
	return h.Sum(nil)
}

// refreshFallbacks builds again, when the unlocked keys differ from those the
// stored messages were built with, every stored message that had something
// left encrypted: a key unlocked since may open it, and it then gets new
// UIDs (refreshMessage), one at a time, behind the background download's
// pace. It stops when ctx ends or a message fails for a reason that may
// pass (the network, Proton throttling); the digest is stored only once all
// are done, so the next login carries on.
func (u *user) refreshFallbacks(ctx context.Context, deliver func([]imapbackend.Update)) {
	digest := u.unlockedKeysDigest()
	stored, err := u.db.UnlockedKeys()
	if err != nil {
		log.Printf("cannot read the keys the messages of %q were built with: %v", u.u.Name, err)
		return
	}
	if bytes.Equal(stored, digest) {
		return
	}
	if stored == nil {
		// None recorded yet: what is stored was built in this process,
		// with these keys.
		if err := u.db.SetUnlockedKeys(digest); err != nil {
			log.Printf("cannot record the keys the messages of %q are built with: %v", u.u.Name, err)
		}
		return
	}
	ids, err := u.db.Fallbacks()
	if err != nil {
		log.Printf("cannot list the messages of %q to build again: %v", u.u.Name, err)
		return
	}
	if len(ids) > 0 {
		log.Printf("The unlocked keys of %q changed: building %v messages again", u.u.Name, len(ids))
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := u.refreshMessage(id, deliver); protonmail.Retryable(err) {
			// Left as stored; tried again at the next login.
			log.Printf("cannot build message %v again: %v", id, err)
			return
		} else if err != nil {
			log.Printf("cannot build message %v again: %v", id, err)
		}
	}
	if err := u.db.SetUnlockedKeys(digest); err != nil {
		log.Printf("cannot record the keys the messages of %q are built with: %v", u.u.Name, err)
	}
}

// gone reports whether err says Proton has no such message (code 2501).
func gone(err error) bool {
	apiErr, ok := errors.AsType[*protonmail.APIError](err)
	return ok && apiErr.Code == protonmail.CodeMessageDoesNotExist
}
