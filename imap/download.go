package imap

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// downloadRetryDelay is how long the download waits after a failed page before
// asking again, as proton-bridge's sync (syncservice DefaultRetryCoolDown).
const downloadRetryDelay = 20 * time.Second

// startDownload starts downloading the metadata of every message of the
// account, unless the database has it all, as proton-bridge syncs at login.
// The download stops when it is done, when the last client logs out (u.done)
// or when stopDownload is called. Called by newUser and by the events
// goroutine only, which own u.downloadStop.
func (u *user) startDownload() {
	stop := make(chan struct{})
	u.downloadStop = stop
	u.downloads.Go(func() { u.download(stop, u.sendUpdates) })
}

// stopDownload stops the download and waits for it to end: at most for the
// page it is getting.
func (u *user) stopDownload() {
	if u.downloadStop == nil {
		return
	}
	close(u.downloadStop)
	u.downloadStop = nil
	u.downloads.Wait()
}

// download stores the account's messages page after page, newest first, from
// where the last page stored left off, until Proton has no older message. Each
// page is asked for and stored while holding syncSem, so that an event applied
// between pages can't be undone by a page fetched before it; events and actions
// wait for one page at most. send gets each page's updates, so that clients
// see the mailboxes fill. It reports whether every message is stored.
func (u *user) download(stop <-chan struct{}, send func([]imapbackend.Update)) (done bool) {
	for {
		updates, pageStored, finished := u.downloadPage(stop)
		if finished {
			_, done, err := u.db.SyncState()
			return err == nil && done
		}
		if pageStored {
			send(updates)
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

// downloadPage asks for and stores the next page. It reports finished once
// there is nothing left to do (all stored, or stopped), and pageStored when
// it stored a page, with the updates telling clients about it; neither means
// it failed and should be tried again later.
func (u *user) downloadPage(stop <-chan struct{}) (updates []imapbackend.Update, pageStored, finished bool) {
	if !u.lockSyncUnless(stop) {
		return nil, false, true
	}
	cursor, done, err := u.db.SyncState()
	if err != nil {
		u.unlockSync()
		log.Printf("cannot read where the download of %q stands: %v", u.u.Name, err)
		return nil, false, false
	}
	if done {
		u.unlockSync()
		return nil, false, true
	}

	page, err := u.c.MessageMetadataPage(cursor, protonmail.MetadataPageSize)
	if err != nil {
		u.unlockSync()
		log.Printf("cannot download messages of %q before %q: %v (retrying in %v)", u.u.Name, cursor, err, downloadRetryDelay)
		return nil, false, false
	}
	// The message the page ends at comes first if Proton still has it.
	if len(page) > 0 && cursor != "" && page[0].ID == cursor {
		page = page[1:]
	}
	if len(page) == 0 {
		err := u.db.SetSyncDone()
		u.unlockSync()
		if err != nil {
			log.Printf("cannot record that the download of %q is done: %v", u.u.Name, err)
			return nil, false, false
		}
		log.Printf("Downloaded every message of %q", u.u.Name)
		return nil, false, true
	}

	changes, err := u.db.SyncPage(page)
	if err == nil {
		updates = u.downloadUpdates(changes)
	}
	u.unlockSync()
	if err != nil {
		log.Printf("cannot store downloaded messages of %q: %v (retrying in %v)", u.u.Name, err, downloadRetryDelay)
		return nil, false, false
	}
	log.Printf("Downloaded %v messages of %q, down to %v", len(page), u.u.Name, page[len(page)-1].ID)
	return updates, true, false
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
// (SIGINT), it returns an error, and the next download or login resumes.
func DownloadAccount(c *protonmail.Client, discard bool) error {
	pu, err := c.GetCurrentUser()
	if err != nil {
		return fmt.Errorf("cannot get the account: %w", err)
	}
	db, err := database.Open(pu.Name + ".db")
	if err != nil {
		return err
	}

	u := &user{
		c:        c,
		u:        pu,
		username: pu.Name,
		db:       db,
		messages: newMessageCache(),
		syncSem:  make(chan struct{}, 1),
		done:     make(chan struct{}),
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
	signal.Notify(interrupt, os.Interrupt)
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
	done := u.download(stop, func([]imapbackend.Update) {})
	close(finished)
	if err := db.Close(); err != nil {
		return fmt.Errorf("cannot close the database: %w", err)
	}
	if !done {
		return errors.New("the download was stopped before every message was stored")
	}
	return nil
}
