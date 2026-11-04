# TODO

Work left for a later session, found while fixing unread counts and
latency in October 2026. Each item says what is wrong, where, what was
observed, and the steps a fix needs.

## 1. Folders and labels: names, and what `Labels` events leave open

`Labels` events are handled (`user.handleLabelEvent`, imap/user.go):
folders and labels created, renamed or deleted in Proton change
`u.mailboxes` and `u.flags` without a restart. Clients are not told:
they see the change on their next LIST (aerc lists only after login, so
after it reconnects). go-imap can send an unsolicited LIST
(`backend.MailboxInfoUpdate`), but its client, which aerc uses, drops it.

**Observed.**

- 2026-10-14 `-debug-raw` log: entries look like
  `{"ID": ..., "Action": 2, "Label": {...}}`. An update for "Primary",
  "All Mail" and "Inbox" (`Type` 1, `Exclusive` 0) followed every
  read/unread toggle, changing only `LastUnseenMessageEventID`. System
  labels are therefore skipped, and updates act only on a changed name.
- 2026-11-04 `-debug-raw` log (bob): `GET /labels`, with no
  type, lists custom folders and labels alike, both `Type` 1: folder
  `PEROXIDE_FOLDER` with `Exclusive` 1, labels `peroxide_label_1..3`
  with `Exclusive` 0. `Path` equals `Name` for these top-level ones.
  No `Labels` event was in that log: create, rename and delete events
  are only checked against the 2026-10-14 shape and go-proton-api.

**Left open.**

1. Mailbox names. peroxide names a folder after `Name` alone, flat
   (`\Noinferiors`), and a label becomes the flag
   `labelNameToFlag(Name)`:
   - Nested folders show flat. `Path` is one string joined with `/`
     (go-proton-api splits it); peroxide decodes neither `Path` nor
     `ParentID`.
   - Two folders with the same name, at different levels or matching a
     system mailbox ("Sent"), collide in `user.getMailbox`, which picks
     one in map order. Two labels mapping to the same flag ("To do" and
     "To_do") collide the same way in `user.getFlag`.
   - proton-bridge names folders `Folders/<Path>` and labels
     `Labels/<Path>`, labels being mailboxes there, not flags
     (`GetMailboxName`, internal/services/imapservice/imap_updates.go).
     That removes every collision, and aerc treats a server on 127.0.0.1
     as Proton and mailboxes under `Labels/` as labels
     (worker/imap/list.go), so it would gain label support.
   - Cost: it renames every custom mailbox, breaking client settings
     that name them (aerc `folders`, `archive`, `copy-to`). Local data is
     keyed by label ID, so no resync. Labels as mailboxes also need COPY
     to label and EXPUNGE to unlabel. A parent's rename sends no event
     for its children: proton-bridge fetches each child again
     (`onLabelUpdated`).
2. A folder that becomes a label, or back (`Exclusive` changes in an
   update), is logged and ignored until the next login. proton-bridge
   never handles it either, so Proton probably never sends it. If it
   does, handle it as a delete then a create.
3. A folder deleted while a client has it selected gives that client
   errors on its next command, instead of a BYE or a closed mailbox.
4. Check live: create, rename and delete a folder and a label while
   peroxide runs with `-debug-raw` (capture with `2>&1`: API logs go to
   stderr, IMAP logs to stdout), and check the event shape and the logs
   `Folder ... created/renamed/deleted`.

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

## 4. Debug Raw

Make an educated decision into droping `-debug` in favour of `-debug-raw`. The raw dump of debug with API keys scrubbed from the flag '-debug-raw' has proven invaluable to all previous implementations, whilst '-debug' only presents a security threat.

## 5. Import / Export

Perform an analysis of what schema is utilised in both procedures and verify if it is currently compliant with Proton API.

## 6. Captcha solution

Attempt a more elegant workaround or optimise the Captcha implementation.
