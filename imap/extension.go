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

var _ imapserver.ConnExtension = Extension{} // proof: compile-time

func (Extension) Capabilities(imapserver.Conn) []string {
	return nil
}

// NewConn hands the backend the server, so that it can disconnect an
// account's clients (backend.disconnect).
func (Extension) NewConn(c imapserver.Conn) imapserver.Conn {
	if be, ok := c.Server().Backend.(*backend); ok {
		be.setServer(c.Server())
	}
	return c
}

// Command replaces FETCH, SELECT and EXAMINE. UID FETCH looks its inner
// command up by the same name (go-imap server.Uid), so it gets this one too.
func (Extension) Command(name string) imapserver.HandlerFactory {
	switch name {
	case "FETCH":
		return func() imapserver.Handler { return new(fetchCommand) }
	case "SELECT":
		return func() imapserver.Handler { return new(selectCommand) }
	case "EXAMINE":
		return func() imapserver.Handler {
			cmd := new(selectCommand)
			cmd.ReadOnly = true
			return cmd
		}
	}
	return nil
}

// selectCommand is go-imap's SELECT, which then tells the account which
// mailbox a client opened, so that the background download stores its
// messages first: the backend can't tell a SELECT from a STATUS.
type selectCommand struct {
	imapserver.Select
}

func (cmd *selectCommand) Handle(conn imapserver.Conn) error {
	if err := cmd.Select.Handle(conn); err != nil {
		return err
	}
	if mbox, ok := conn.Context().Mailbox.(*mailbox); ok {
		mbox.u.prioritize(mbox.label)
	}
	return nil
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
