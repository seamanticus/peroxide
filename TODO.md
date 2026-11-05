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
- 2026-11-04/05 `-debug-raw` logs: a folder created (`Action` 1),
  renamed (`Action` 2) and deleted (`Action` 0, `ID` only) arrived at the
  next poll and were applied. The event `ID` of system-label updates
  differs from `Label.ID`; for custom folders it was the same.

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
4. Check live with a label (not a folder): create, rename and delete one
   while peroxide runs with `-debug-raw` (capture with `2>&1`: API logs
   go to stderr, IMAP logs to stdout), and check the logs
   `Label ... created/renamed`. Folders were checked on 2026-11-04/05.

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

## 3. APPEND only into Drafts

**Problem.** `mailbox.CreateMessage` (imap/mailbox.go) refuses APPEND
outside Drafts ("cannot create messages outside the Drafts mailbox"), a
rule of peroxide's own since 2018 (commit 2a1b998), not of Proton's. So a
client can't copy or move a message from another account into a Proton
folder, nor save a sent copy into Sent: aerc tried `APPEND INBOX` on
2026-11-04 and got NO.

**How proton-bridge does it** (`Connector.CreateMessage`,
internal/services/imapservice/connector.go): every mailbox but All Mail
accepts APPEND, through Proton's import API (`ImportMessages`,
go-proton-api message_import.go), with:

- label IDs: the mailbox, plus Starred if `\Flagged` is set;
- flags: Received into INBOX, Sent into Sent; elsewhere Received if the
  message has a `Received` header, Sent otherwise; Replied if
  `\Answered`; Drafts keeps its own draft path (peroxide's `createMessage`);
- unread unless `\Seen` (always read in Drafts);
- size: at most 50 MB once encrypted (`ErrImportSizeExceeded`), the
  APPENDLIMIT peroxide already announces;
- a message just sent over SMTP is matched by hash (`sendRecorder`) and
  not imported twice into Sent.

**Steps.**

1. peroxide has an import client (`protonmail.Client.Import`, used by
   imports/messages.go): reuse it for APPEND, with the metadata above.
2. Refuse All Mail (and Starred, a flag here) with NO, as bridge does.
3. Decide on the SMTP duplicate check: peroxide's SMTP server sends
   through a Proton draft (`CreateDraftMessage` then `SendMessage`,
   smtp/smtp.go), which Proton files in Sent, so a client that also
   APPENDs to Sent (aerc `copy-to`) would create a second copy.
4. Tests (imap): the label IDs and flags sent for INBOX, Sent, a custom
   folder and Drafts; All Mail refused; a literal over APPENDLIMIT gets
   TOOBIG before any request.

## 4. Debug Raw

Make an educated decision into droping `-debug` in favour of `-debug-raw`. The raw dump of debug with API keys scrubbed from the flag '-debug-raw' has proven invaluable to all previous implementations, whilst '-debug' only presents a security threat.

## 5. Import / Export

Perform an analysis of what schema is utilised in both procedures and verify if it is currently compliant with Proton API.

## 6. Captcha solution

Attempt a more elegant workaround or optimise the Captcha implementation.

## 7. Unlock keys added by `Addresses` events

**Problem.** `user.handleAddressEvent` (imap/addresses.go) applies
addresses created, changed and deleted in Proton to the address list, but
not their keys. Private keys are unlocked once per peroxide process, by
`protonmail.Client.Unlock` at the first login of the account, and
`auth.Manager` caches that session (auth/auth.go, `sessions`). A key
added later (a new address, a key rotation) is logged as "is new: it is
unlocked when peroxide restarts" and stays locked until then: mail
encrypted to it fails to decrypt, and drafts from that address fail with
"sender address key hasn't been decrypted". The SMTP session's address
list (smtp/smtp.go, `session.addrs`) isn't refreshed at all.

**Constraint.** Don't keep the mailbox password, or anything that
unlocks keys, in memory for the session.

**How proton-bridge does it**: it stores the salted key password in its
encrypted vault (internal/vault/user.go, `KeyPass`), reads it whenever a
key ring is needed and unlocks the address keys on demand
(internal/services/useridentity/state.go, `WithAddrKR`), so a new key
works at once. The vault stays encrypted in memory (`vault.enc`) but is
decrypted on every read with an AES-GCM key bridge holds for its whole run
(internal/vault/vault.go, `New`, `getUnsafe`): the key that unlocks
everything is in memory, the trade-off ruled out above.

**Steps.**

1. Pick a way to unlock on an event without holding the secret, e.g.
   re-reading it from the encrypted `auth.json` with a key the client
   proves again, or deferring to the next IMAP/SMTP login that carries
   the bridge password. To be decided.
2. Unlock the new keys of `event.Address` with the user key ring and
   add them to the session's key ring (`auth.session.privateKeys`), so
   IMAP and SMTP see them.
3. Refresh the SMTP session's address list from the same events.
4. Tests: an address event with a new key makes it usable for
   decryption without restarting.
