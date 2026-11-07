package imap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/auth"
	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// downloadRetryDelay is how long the download waits after a failed page before
// asking again, as proton-bridge's sync (syncservice DefaultRetryCoolDown).
const downloadRetryDelay = 20 * time.Second

// startDownload starts downloading every message of the account, its
// metadata and then its body, unless the database has it all, as
// proton-bridge syncs at login. The download stops when it is done, when the
// last client logs out (u.done) or when stopDownload is called. Messages in
// the failed set aren't tried again. Called by newUser and by the events
// goroutine only, which own u.downloadStop.
func (u *user) startDownload() {
	stop := make(chan struct{})
	u.downloadStop = stop
	deliver := func(ups []imapbackend.Update) { u.sendOn(u.backend.updates, ups) }
	u.downloads.Go(func() { u.download(stop, deliver, false) })
}

// stopDownload stops the download, aborting the request it is making, and
// waits for it to end.
func (u *user) stopDownload() {
	if u.downloadStop == nil {
		return
	}
	close(u.downloadStop)
	u.downloadStop = nil
	u.downloads.Wait()
}

// download stores the account's messages page after page, newest first, from
// where the last page stored left off, until Proton has no older message, as
// proton-bridge's sync. A page is asked for without syncSem, so that a slow
// or hung request keeps nothing else waiting, and stored under it; deliver
// gets its updates, so that clients see the mailboxes fill. An event applied
// between the request and the storing can be undone by the page (a message
// deleted comes back, flags go back): so, as bridge does, the event ID of
// when the download began is kept (SetSyncStart), and once every message is
// stored the events after it are applied again. Then it downloads the
// bodies (downloadBodies), trying failed messages again if retryFailed. It
// reports whether every message, metadata and body, is stored, failed ones
// aside unless retried.
func (u *user) download(stop <-chan struct{}, deliver func([]imapbackend.Update), retryFailed bool) (done bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
		case <-u.done:
		case <-ctx.Done():
		}
		cancel()
	}()

	for {
		pageStored, finished := u.downloadPage(ctx, stop, deliver)
		if finished {
			s, err := u.db.SyncState()
			if err != nil || !s.Done {
				return false
			}
			u.refreshFallbacks(ctx, deliver)
			return u.downloadBodies(ctx, retryFailed)
		}
		if pageStored {
			continue
		}
		select {
		case <-time.After(downloadRetryDelay):
		case <-stop:
			return false
		case <-u.done:
			return false
		}
	}
}

// downloadPage asks for and stores the next page, and has deliver tell
// clients what it changed. It reports finished once there is nothing left to
// do (all stored, or stopped), and pageStored when it stored a page; neither
// means it failed and should be tried again later.
func (u *user) downloadPage(ctx context.Context, stop <-chan struct{}, deliver func([]imapbackend.Update)) (pageStored, finished bool) {
	// Only the download writes the cursor: reading it without syncSem is
	// safe, and a Wipe stops the download first.
	state, err := u.db.SyncState()
	if err != nil {
		log.Printf("cannot read where the download of %q stands: %v", u.u.Name, err)
		return false, false
	}
	if state.Done {
		return false, true
	}
	if !state.Started {
		// Before the first request: every event applied after this one may
		// be undone by a page fetched before it. Under syncSem, so that no
		// event page is half applied. SetSyncStart keeps the first start of
		// a download stopped before its first page was stored.
		if !u.lockSyncUnless(stop) {
			return false, true
		}
		lastEventID, _, err := u.db.LastEventID()
		if err == nil {
			err = u.db.SetSyncStart(lastEventID)
		}
		u.unlockSync()
		if err != nil {
			log.Printf("cannot record where the events stand as the download of %q begins: %v (retrying in %v)", u.u.Name, err, downloadRetryDelay)
			return false, false
		}
	}

	// No cursor before the first page: the newest messages are asked for.
	page, err := u.c.MessageMetadataPage(ctx, state.Cursor, protonmail.MetadataPageSize)
	if ctx.Err() != nil {
		return false, true // stopped
	}
	if err != nil {
		log.Printf("cannot download messages of %q before %q: %v (retrying in %v)", u.u.Name, state.Cursor, err, downloadRetryDelay)
		return false, false
	}
	// The message the page ends at comes first if Proton still has it.
	if len(page) > 0 && state.Started && page[0].ID == state.Cursor {
		page = page[1:]
	}

	if !u.lockSyncUnless(stop) {
		return false, true
	}
	if len(page) == 0 {
		startEventID, err := u.db.SetSyncDone()
		if err != nil {
			u.unlockSync()
			log.Printf("cannot record that the download of %q is done: %v", u.u.Name, err)
			return false, false
		}
		if startEventID != "" {
			// The pages after it come again, the receiver asking from there
			// at its next poll, which is asked for now.
			u.rewindEvents(startEventID)
		}
		u.unlockSync()
		u.pollSoon()
		log.Printf("Downloaded every message of %q", u.u.Name)
		return false, true
	}

	changes, err := u.db.SyncPage(page)
	var updates []imapbackend.Update
	if err == nil {
		updates = u.downloadUpdates(changes)
	}
	u.unlockSyncAndSend(updates, deliver)
	if err != nil {
		log.Printf("cannot store downloaded messages of %q: %v (retrying in %v)", u.u.Name, err, downloadRetryDelay)
		return false, false
	}
	log.Printf("Downloaded %v messages of %q, down to %v", len(page), u.u.Name, page[len(page)-1].ID)
	return true, false
}

// downloadUpdates tells clients what a page changed, in the order it changed
// it: an EXPUNGE per message that left a mailbox, and per mailbox one EXISTS
// with its count, sent before any EXPUNGE that the count must precede.
// Mailboxes in label ID order.
func (u *user) downloadUpdates(changes []database.MessageChange) []imapbackend.Update {
	var updates []imapbackend.Update
	exists := make(map[string]uint32) // not sent yet, by label ID
	flush := func(labelID string) {
		count, ok := exists[labelID]
		if !ok {
			return
		}
		delete(exists, labelID)
		if mbox := u.getMailboxByLabel(labelID); mbox != nil {
			up := new(imapbackend.MailboxUpdate)
			up.Update = newMailboxUpdate(u.u.Name, mbox)
			up.MailboxStatus = imap.NewMailboxStatus(mbox.Name(), []imap.StatusItem{imap.StatusMessages})
			up.MailboxStatus.Messages = count
			updates = append(updates, up)
		}
	}
	for _, change := range changes {
		for _, labelID := range slices.Sorted(maps.Keys(change.Removed)) {
			flush(labelID)
			if mbox := u.getMailboxByLabel(labelID); mbox != nil {
				up := new(imapbackend.ExpungeUpdate)
				up.Update = newMailboxUpdate(u.u.Name, mbox)
				up.SeqNum = change.Removed[labelID]
				updates = append(updates, up)
			}
		}
		// The last sequence number added is the new count.
		maps.Copy(exists, change.Added)
	}
	for _, labelID := range slices.Sorted(maps.Keys(exists)) {
		flush(labelID)
	}
	return updates
}

// DownloadAccount downloads the account c is logged in to into its local
// database, as serve does in the background after a login, and returns once
// every message is stored. With discard, the database is wiped first, as a
// Refresh does, so that it is rebuilt from Proton alone. It fails with
// database.ErrInUse while peroxide serve has the database open. Interrupted
// (SIGINT, SIGTERM), it aborts the request it is making and returns an error,
// and the next download or login resumes. The events received meanwhile are
// applied at the next login: the download sets the stored event ID back to
// where it began. storeKey seals the stored messages.
// privateKeys decrypt the bodies; messages that failed before are tried
// again.
func DownloadAccount(c *protonmail.Client, privateKeys openpgp.EntityList, storeKey auth.StoreKey, discard bool) error {
	pu, err := c.GetCurrentUser()
	if err != nil {
		return fmt.Errorf("cannot get the account: %w", err)
	}
	addrs, err := c.ListAddresses()
	if err != nil {
		return fmt.Errorf("cannot list the addresses: %w", err)
	}
	db, keyChanged, err := database.Open(pu.Name, storeKey)
	if err != nil {
		return err
	}
	if keyChanged {
		log.Printf("The messages of %q were stored under another bridge password: downloading them again", pu.Name)
	}

	u := &user{
		c:           c,
		u:           pu,
		username:    pu.Name,
		privateKeys: privateKeys,
		addrs:       addrs,
		db:          db,
		syncSem:     make(chan struct{}, 1),
		sendSem:     make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
	if discard {
		log.Printf("Discarding the local copy of %q", pu.Name)
		if err := db.SetNeedsRebuild(); err != nil {
			return errors.Join(fmt.Errorf("cannot mark the database for a rebuild: %w", err), db.Close())
		}
	}
	// As a login does: wipe if asked, take the event ID to resume from,
	// make the mailboxes, so that the download files messages in them.
	if _, err := u.initSession(); err != nil {
		return errors.Join(err, db.Close())
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		select {
		case <-interrupt:
			log.Printf("Interrupted: the download resumes from here next time")
			close(stop)
		case <-finished:
		}
	}()

	// No client is connected: the updates are for no one.
	done := u.download(stop, func([]imapbackend.Update) {}, true)
	close(finished)
	if err := db.Close(); err != nil {
		return fmt.Errorf("cannot close the database: %w", err)
	}
	if !done {
		return errors.New("not every message was stored: stopped, or some failed (logged above); run again to resume")
	}
	return nil
}
