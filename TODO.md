# TODO

Work left for a later session, found while fixing unread counts and
latency in October 2026. Each item says what is wrong, where, what was
observed, and the steps a fix needs.

## 1. Handle `Labels` events

**Problem.** Folders and labels are read once, at login, by
`user.initMailboxes` (imap/user.go). Proton's events carry a `Labels`
list, but `protonmail.Event` doesn't decode it (`//Labels` in
protonmail/events.go). A folder or label created, renamed or deleted in
Proton therefore only shows up in IMAP after peroxide restarts.

**Observed** in a `-debug-raw` log (2026-10-14, 3 accounts):

- Entries look like `{"ID": ..., "Action": 2, "Label": {...}}`, with
  `Label` holding `ID, Name, Path, Type, Exclusive, Color, Display,
  Notify, Order, LastUnseenMessageEventID`. `ParentID` was null.
- An update (`Action` 2) for "Primary", "All Mail" and "Inbox" (`Type` 1,
  `Exclusive` 0) followed every read/unread toggle: 17 times, each time
  after a STORE. Only `LastUnseenMessageEventID` changed.
- `GET /labels` returned no labels for any of the three accounts, so
  there was no custom folder or label to test with.

**Steps.**

1. protonmail: add `Labels []*EventLabel` to `Event`, with
   `EventLabel{ID string; Action EventAction; Label *Label}`, and
   `Path`, `ParentID` to `Label`. Check the actions are numbered as for
   messages (0 delete, 1 create, 2 update).
2. imap/user.go, `receiveEvents`: for each label event, under `u.Lock()`:
   - create: an exclusive label (folder) becomes a mailbox
     (`newMailbox(name, id, nil, u)` into `u.mailboxes`), any other a flag
     (`u.flags[id] = labelNameToFlag(name)`), as `initMailboxes` does;
   - update: act only if `Name`, `Path` or `Exclusive` changed, as updates
     fire on every unread change; rename the mailbox or flag;
   - delete: drop the mailbox or flag, and its bucket in the local
     database (needs a new `database` function; `Mailbox.Reset` recreates
     the bucket).
3. Decide how clients learn about it. go-imap can send an unsolicited LIST
   (`backend.MailboxInfoUpdate`); check whether aerc uses it or only
   re-lists on its own.
4. Decide on mailbox names. peroxide names mailboxes after `Name` alone,
   flat (`\Noinferiors`): nested folders show flat, and two folders with
   the same name collide in `user.getMailbox`, which picks one at random
   (map order). Naming them after `Path` fixes both, but renames
   mailboxes for existing users.
5. Check the label list itself: Proton's client asks
   `/core/v4/labels?Type=1`, `Type=3` and `Type=4` separately, peroxide
   asks `/labels` with no type. Create a custom folder and a custom label
   in Proton and see whether `GET /labels` lists them; if not, ask per
   type.
6. Tests (imap): events creating, renaming and deleting a folder and a
   label; an update changing only `LastUnseenMessageEventID` changes
   nothing and sends no update.

## 2. `STORE +FLAGS (\Deleted)` without `.SILENT` sends no FETCH

**Problem.** In `mailbox.UpdateMessagesFlags` (imap/mailbox.go), the
`imap.DeletedFlag` case (`// TODO: send updates`) only records the flag
in memory (`mbox.deleted`) and tells no client. RFC 3501 6.4.6: "The new
value of the flags is returned as if a FETCH of those flags was done",
unless `.SILENT` is used. go-imap only sends that FETCH itself when the
backend has no updates channel (`Store.handle` in
go-imap/server/cmd_selected.go), and peroxide has one.

aerc always uses `+FLAGS.SILENT (\Deleted)`, so it isn't affected. A
client that doesn't would not see the message as deleted until it
fetches flags again.

**Steps.**

1. After changing `mbox.deleted`, build a `backend.MessageUpdate` per
   message, as `user.applyUpdate` (imap/actions.go) does: sequence number
   and UID from `mbox.db.FromApiID`, flags from `mbox.fetchFlags`. Send
   them with `user.sendUpdates`. go-imap drops them for the connection
   that sent a `.SILENT` STORE (`conn.silent`), so aerc sees no change.
2. `mbox.deleted` belongs to the mailbox, shared by its connections:
   other connections with the mailbox selected get the FETCH too, which
   is right.
3. Same switch, `// TODO: imap.SetFlags should remove currently set
   flags`: `STORE FLAGS (...)` must also clear the flags not listed
   (`\Seen`, `\Deleted`, labels). Worth fixing in the same commit.
4. Document that `\Deleted` isn't sent to Proton and is lost when peroxide
   restarts; IMAP allows that.
5. Tests (imap/actions_test.go): non-silent `+FLAGS (\Deleted)` sends one
   FETCH with `\Deleted` and the UID; `-FLAGS (\Deleted)` sends one
   without it; no API request is made.

## 3. Background polls can leave an idle goroutine after logout

**Problem.** `Receiver.Poll` (events/events.go) sends on an unbuffered
channel, which `Receiver.receiveEvents` only reads while waiting between
polls. `user.pollSoon` (imap/actions.go), called after every STORE, COPY,
MOVE and EXPUNGE, and `user.poll` (imap/user.go) call it with `go`. Once
the last client logs out, the receiver fetches one more event, finds no
channel (`if n == 0 { break }`) and returns without reading the poll
channel again: a `Poll` still pending then blocks forever, keeping its
goroutine and the `Receiver` alive.

**Steps.**

1. Make the poll channel buffered (capacity 1) and `Poll` non-blocking:
   `select { case r.poll <- struct{}{}: default: }`. A pending request
   already covers a new one, so nothing is lost. Callers then drop the
   `go`.
2. `user.poll` (used by NOOP and APPEND) then waits on `u.eventSent`,
   which a pending poll still signals. It also blocks forever if the
   receiver stopped, or while `GetEvent` keeps failing (that path
   delivers nothing): wait with a timeout, or also on the user's `done`.
3. Related race, same file: `Manager.Register` can add a new user's
   channel to a receiver that just found no channel and is about to
   return, but hasn't left `m.receivers` yet. That user then never gets
   events. Remove the receiver from `m.receivers` under `m.locker` before
   it returns, and let `Register` start a new one.
4. Tests (events): with no reader, `Poll` returns at once; a receiver
   whose last channel was removed accepts no new channel (`Register`
   starts a new receiver). Don't count goroutines, which is flaky.
