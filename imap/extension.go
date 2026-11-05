package imap

import (
	"slices"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
	imapserver "github.com/emersion/go-imap/server"
)

// Extension adds to go-imap's server what its backend interface can't carry.
// Enable it on the server that serves this backend (Server.Enable). NOTIFY
// (notify_design.md) plugs in here too.
type Extension struct{}

var _ imapserver.Extension = Extension{} // proof: compile-time

func (Extension) Capabilities(imapserver.Conn) []string {
	return nil
}

// Command replaces FETCH. UID FETCH looks its inner command up by the same
// name (go-imap server.Uid), so it gets this one too.
func (Extension) Command(name string) imapserver.HandlerFactory {
	if name != "FETCH" {
		return nil
	}
	return func() imapserver.Handler { return new(fetchCommand) }
}

// fetchCommand is go-imap's FETCH, plus what the backend can't learn from
// Mailbox.ListMessages: whether the mailbox was opened read-only (EXAMINE),
// where fetching a body must not set \Seen (RFC 3501 section 6.3.2).
type fetchCommand struct {
	commands.Fetch
}

func (cmd *fetchCommand) Handle(conn imapserver.Conn) error {
	return cmd.handle(false, conn)
}

func (cmd *fetchCommand) UidHandle(conn imapserver.Conn) error {
	// Clients take only responses bearing the UID (RFC 3501 section 6.4.8).
	if !slices.Contains(cmd.Items, imap.FetchUid) {
		cmd.Items = append(cmd.Items, imap.FetchUid)
	}
	return cmd.handle(true, conn)
}

func (cmd *fetchCommand) handle(uid bool, conn imapserver.Conn) error {
	ctx := conn.Context()
	if ctx.Mailbox == nil {
		return imapserver.ErrNoMailboxSelected
	}
	mbox, ok := ctx.Mailbox.(*mailbox)
	if !ok {
		panic("invariant: the selected mailbox comes from peroxide's backend")
	}

	ch := make(chan *imap.Message)
	written := make(chan error, 1)
	go func() {
		written <- conn.WriteResp(&responses.Fetch{Messages: ch})
		for range ch {
			// WriteResp stopped early: let fetch finish.
		}
	}()

	// fetch closes ch whatever happens, so the writer always returns: wait
	// for it, so that the tagged response follows the FETCH responses.
	err := mbox.fetch(uid, cmd.SeqSet, cmd.Items, ch, !ctx.MailboxReadOnly)
	writeErr := <-written
	if err != nil {
		return err
	}
	return writeErr
}
