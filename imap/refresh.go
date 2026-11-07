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

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// refreshMessage builds again the stored message apiID, whose contents may
// have changed (an edited draft, a key unlocked since), as gluon does
// (applyMessageUpdated): if the bytes built differ from those stored, the
// message gets new UIDs with the new bytes, and the updates telling clients
// (EXPUNGE of the old, EXISTS of the new) are returned; if they are the same,
// nothing changes. A message not stored keeps its UIDs: its next FETCH builds
// what Proton has then, and a build of it running now isn't stored. Called
// with syncSem; the request is bounded by unknownMessageTimeout.
//
// A failure that retrying soon may fix (retryable) is returned, to try the
// event again. Another drops what is stored and puts the message in the
// failed set: what is served may then change under a UID, which beats
// serving an old draft for good.
func (u *user) refreshMessage(apiID string) ([]imapbackend.Update, error) {
	// Owning the build keeps FETCHes from building it meanwhile.
	_, owner := u.builds.start(apiID)
	if !owner {
		// Being built, so not stored: what the build downloaded may be
		// older than this event.
		return nil, u.builds.drop(u.db, apiID)
	}
	defer u.builds.finish(apiID)

	_, old, err := u.db.Body(apiID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), unknownMessageTimeout)
	defer cancel()
	built, err := u.downloadAndBuild(ctx, apiID)
	if errors.Is(err, errMessageGone) {
		return nil, nil // its delete event follows
	} else if retryable(err) {
		return nil, err
	} else if err != nil {
		log.Printf("Message %v changed and can't be built again: dropped until a FETCH tries again: %v", apiID, err)
		return nil, errors.Join(u.db.DeleteBody(apiID), u.db.AddFailed(apiID))
	}
	if bytes.Equal(built.literal, old) {
		return nil, nil
	}

	log.Printf("Message %v changed: it gets new UIDs", apiID)
	r, err := u.db.RenewMessage(apiID, built.literal, built.info)
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("cannot give message %v new UIDs: %w", apiID, err)
	}
	return u.renewalUpdates(r), nil
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
// no longer has is errMessageGone.
func (u *user) downloadAndBuild(ctx context.Context, apiID string) (builtMessage, error) {
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
// UIDs (refreshMessage). Each takes syncSem and delivers its updates in
// turn. It stops when ctx ends; the digest is stored only once all are done,
// so the next login carries on.
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
		u.lockSync()
		updates, err := u.refreshMessage(id)
		u.unlockSyncAndSend(updates, deliver)
		if err != nil {
			// Left as stored; tried again at the next login.
			log.Printf("cannot build message %v again: %v", id, err)
			return
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
