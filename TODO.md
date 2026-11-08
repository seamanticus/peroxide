# TODO

Work left for a later session, found while fixing unread counts and
latency in October 2026. Each item says what is wrong, where, what was
observed, and the steps a fix needs.

## 1. Folders and labels: names, and what `Labels` events leave open

**Done (labels-events: ecddff4, 058cd3a, aeddcb7, ca6d297; bc32862).**
Folders and labels created, renamed or deleted in Proton are applied
from `Labels` events without a restart; system-label updates are
skipped by label ID; FLAGS lists labels in label-ID order. The items
under "Left open" remain.

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
     system mailbox ("Sent"), collide in `user.getMailbox`
     (imap/user.go), which ranges over `u.mailboxes` and returns the
     first match in map order: which one a SELECT opens can change from
     one command to the next. `user.ListMailboxes` lists them in map
     order too (LIST output order varies; harmless alone, but the
     duplicate name appears twice). Two labels mapping to the same flag
     ("To do" and "To_do", or since 1dfcddc "Work" and "work", as flags
     now match without case) collide in `user.getFlag`, which since
     1dfcddc tries labels in sorted ID order, so it is deterministic but
     still reaches one label only. Sorting `getMailbox` the same way
     would make SELECT stable without removing the collision; the naming
     below removes it.
   - A folder named "Inbox", "inbox" or any case of INBOX can't be
     opened: go-imap rewrites that name to `INBOX` (RFC 3501 section
     5.1, `imap.CanonicalMailboxName`) in SELECT, EXAMINE, STATUS, COPY,
     APPEND, CREATE, DELETE and RENAME (go-imap commands/*.go) before the
     backend sees it, so every one reaches the real INBOX. LIST still
     shows the folder. The `Folders/` prefix below removes this too.
   - proton-bridge names folders `Folders/<Path>` and labels
     `Labels/<Path>`, labels being mailboxes there, not flags
     (`GetMailboxName`, internal/services/imapservice/imap_updates.go).
     That removes every collision, and aerc treats a server on 127.0.0.1
     as Proton and mailboxes under `Labels/` as labels
     (worker/imap/list.go), so it would gain label support.
   - Cost: it renames every custom mailbox, breaking client settings
     that name them (aerc `folders`, `archive`, `copy-to`). Local data is
     keyed by label ID, so no resync. Labels as mailboxes also need COPY
     to label (EXPUNGE already unlabels, 46d246c). A parent's rename
     sends no event for its children: proton-bridge fetches each child
     again (`onLabelUpdated`).
2. A folder that becomes a label, or back (`Exclusive` changes in an
   update), is logged and ignored until the next login. proton-bridge
   never handles it either, so Proton probably never sends it. If it
   does, handle it as a delete then a create.
3. A folder deleted while a client has it selected gives that client
   errors on its next command, instead of a BYE or a closed mailbox.
   Updates still reach it: `mailboxUpdate.Mailbox` (imap/actions.go)
   reads the deleted `*mailbox`'s name when go-imap delivers, so a
   client with that name selected gets EXISTS or EXPUNGE for a mailbox
   whose bucket is gone, and its next FETCH fails with "cannot find
   mailbox bucket" (`database.Mailbox.bucket`). Fix with the BYE: on
   delete, send the connections that have it selected an unsolicited
   `BYE` (go-imap has no backend update for it; it needs the
   `Extension` of imap/extension.go to keep each connection) or answer
   their commands NO [NONEXISTENT] (RFC 5530). Test: an update made after
   the delete reaches no client.
4. Check live with a label (not a folder): create, rename and delete one
   while peroxide runs with `-debug`, and check the logs
   `Label ... created/renamed`. Folders were checked on 2026-11-04/05.

## 2. (done: STORE `\Deleted` sends FETCH, STORE FLAGS replaces, 1dfcddc)

## 3. (done: APPEND into every mailbox but All Mail and Starred, by import; SMTP sends recorded for APPEND into Sent; APPENDLIMIT 70 MiB; a195b04, bd3c765)

## 4. (done: `-debug` is the raw mode, to one stream; described in docs/debug.md; 64febb9, 1f840c6, 509744c, 0a17156, 6a03626)

Left open: the request line logs `req.URL.RequestURI()`
(protonmail/protonmail.go, `newRequest`) unredacted, so query values
reach the log: `Email` of public-key lookups (protonmail/keys.go),
`Address` of `/messages/count`, and `ListMessages` filters. Message and
label IDs are in paths. None is a credential; docs/debug.md says so.
If logs are ever to be shared without editing, redact query values
whose key is `Email` or `Address`, as `redactedJSONFields` does for
bodies. Test: a `GetPublicKeys` request logs `Email=<removed>`.

## 5. (done: import-messages and export-messages removed; clients use APPEND and FETCH; 1a0d637, b170307, 9546a78)

## 6. Captcha solution

Attempt a more elegant workaround or optimise the Captcha implementation.

## 7. Unlock keys added by `Addresses` events

Done so far (da1f6f3): `Addresses` events update the address list; the
keys themselves are still unlocked only at login.

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

Since d6657fd such mail is served, not failed: its body as an armored
text part whose armor Comment says the key isn't unlocked, its
attachments as `<name>.pgp`, and it is no longer passed through as
PGP/MIME (it is encrypted to an account key the user's gpg lacks). A key
that failed to unlock at login (`unlockKeyRing` logs "failed to unlock
key" and skips it) gives the same.

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

## 8. (done: The All Mail Problem; 1f0bfb4, 2e372dd, 1476ca8, cfbc46b)

All Mail (`"5"`) holds every message, so a client sees each one twice,
and before the account download its bucket stayed empty while messages
still listed it, so a STORE in INBOX logged "cannot get sequence number
... in All Mail". `serve -show-all-mail` (default on, as bridge's
`ShowAllMail`) now hides it from LIST only, SELECT and STATUS still
opening it (1f0bfb4); the account download of 1476ca8 fills every
mailbox once per database, All Mail included, so no mailbox is
unsynced (cfbc46b decided it as section 9.1). Tests:
`TestStoreTellsEveryMailbox`, `TestListShowAllMail`.

Left open: Almost All Mail (`"15"`) and the categories (`"24"`...) have
no mailbox; bridge shows none either. Add one only if asked.

## 9. (done: the message store; labels-events f82e989, 5e88ab3, d6657fd, 49b614e; uid-persist e6562e9..b34f15e)

Every FETCH of a body or BODYSTRUCTURE used to download the full message
again (f82e989 passed end-to-end mail through as PGP/MIME with a real
BODYSTRUCTURE; d6657fd made a FETCH never fail on a part it can't
decrypt). Now each message is built once, as bridge's `BuildRFC822Into`,
and sealed with AES-256-GCM under a key HKDF-derived from the bridge
password, never stored, one file per version under
`$XDG_DATA_HOME/peroxide/<user>/` (c12f3d2, c2f4c0b, 516c292). Every
FETCH item but FLAGS, UID and INTERNALDATE is served from the stored
bytes, so content never changes under a UID, sizes are what is served
and ENVELOPE comes from the original header (2e1a0f4); SEARCH reads the
stored message (fef89f1).

After the metadata, a background download stores every body, newest
first and the selected mailbox first, paced by AIMD shared with FETCH,
which builds unstored messages in parallel; one 64 MB memory budget
covers every build, and messages that fail go to a failed set as in
bridge (ecdb1e5, 6ee0c62, 6197a97, c9c6fc1, 0910cfa). Edited drafts and
messages a newly unlocked key opens are rebuilt outside `syncSem` and get
new UIDs only when their bytes differ, as gluon (d95611c, ab3215f).

Body and attachment signatures are checked with the sender's trusted
Proton keys and the verdict written in `X-Peroxide-Signature`
(8bf8ea4, a3e260e, 5ffb4e7, b34f15e; docs/imap/messages.md#signatures).
SMTP signs attachments, encrypts to a recipient's first active key and
signs mail to recipients without a key when "Sign external messages" is
on (2d8b69d, a33d7ce, c7e66f0).

The decisions are in 1ec3084, d855c30, 3621274 and e6562e9; the full
plan as written is in TODO.md at b34f15e; docs/imap/messages.md,
sync.md and security.md describe the result.

Left open:

- Tests not written: an imap-level check that a stored body can't be
  read with another key, and a FETCH BODYSTRUCTURE benchmark.
- A message not stored yet is built before its first content FETCH:
  one request each (Proton has no batch endpoint for full messages).
- The memory budget isn't first come first served: a big message waits
  until the running builds leave it room.
- A draft is rebuilt at each events page that changes it; waiting for it
  to be quiet was decided against for now.
- go-imap's `backendutil` doesn't descend into attached messages:
  `BODY[2.1]` of a `message/rfc822` part answers empty.
- Messages stored before 8bf8ea4 carry no verdict until rebuilt
  (decided: not rebuilt); Proton's contact key pinning isn't read.
- SMTP doesn't read contacts' own sign, encrypt and scheme preferences
  (vCards), nor send PGP/MIME to external recipients with a key.

### 9.1 (done: keep UIDs across sessions; labels-events c3dc632, f52c088; uid-persist 1476ca8, 6019ec9, b2dfad8, 2bbb24d, 3b67642, 6c5f64b, 2b1110c)

Each session resynced every mailbox from empty, renumbering UIDs from 1
under UIDVALIDITY 1, against RFC 3501. Now, as bridge v3, one background
download lists the whole account once per database, newest first and
resumable; UIDs, UIDVALIDITY (from gluon's epoch) and counts persist;
updates reach clients in order; requests hold no lock and events are
rewound and replayed when the download ends; events failing on the
network, 429 or 5xx are retried, others mark the database for a rebuild
(BYE, wipe, download again). `peroxide download-mailbox` runs it from
the command line. docs/imap/sync.md has the details.

Left open (step 8 of the plan): `syncSem` is one slot for the account,
now held per page and per action, not per sync. A semaphore per mailbox
(an event page taking those it touches, in label-ID order) would let a
STORE in INBOX go ahead while Drafts is busy; test with synctest.

## 10. CREATE, DELETE and RENAME are not implemented

**Problem.** `user.CreateMailbox`, `DeleteMailbox` and `RenameMailbox`
(imap/user.go) return `errNotYetImplemented`, and so does
`mailbox.SetSubscribed` (SUBSCRIBE, UNSUBSCRIBE). A client can't create,
delete or rename a folder: on 2026-11-05 aerc's `:mkdir` and `:rmdir`
got `NO not yet implemented`:

    _x4klw CREATE "FOLDER_CREATE_FROM_CLIENT"
    _x4klw NO not yet implemented
    Xi67Lg DELETE "FOLDER_CLIENT_SHUTDOWN"
    Xi67Lg NO not yet implemented

**How proton-bridge does it** (internal/services/imapservice/connector.go,
`CreateMailbox`, `UpdateMailboxName`, `DeleteMailbox`, and
`createFolder`, `createLabel`, `updateFolder`, `updateLabel`): mailbox
names are `Folders/<path>` or `Labels/<name>`; anything else is refused
(`ErrOperationNotAllowed`), so system mailboxes can't be created,
renamed or deleted. Through go-proton-api (label.go):

- CREATE: `POST /core/v4/labels` with `{Name, Color: "#f66", Type,
  ParentID}`. A nested folder needs its parent to exist (looked up by
  path); a label can't be nested.
- RENAME: reads the label for its color (`GET /core/v4/labels?Type=`),
  then `PUT /core/v4/labels/{id}` with `{Name, Color, ParentID}`.
- DELETE: `DELETE /core/v4/labels/{id}`. Proton moves the folder's
  messages out of it itself.
- The result is stored in bridge's label map at once, before the
  `Labels` event arrives, so the next LIST shows it.

**Differences to settle for peroxide.**

1. Naming. peroxide names folders by `Name`, flat, and labels are
   keywords, not mailboxes (section 1.1). So CREATE "X" can only make
   a top-level folder, and there is no IMAP way to create a label
   (a keyword appears with its first STORE). Refuse names containing
   the delimiter `/` until section 1.1 is decided.
2. Label type. bridge sends `Type` 3 (`LabelTypeFolder`) for folders,
   but `GET /labels` returned peroxide's folders as `Type` 1 with
   `Exclusive` 1 (section 1, 2026-11-04). Check with a `-debug` run
   which form `POST /labels` takes (`Type: 3`, or `Type: 1` with
   `Exclusive: 1`), and what the created label looks like in the
   following `Labels` event.
3. Refuse every system mailbox (INBOX, Sent, Drafts, Trash, Spam,
   Archive, All Mail, Starred) with NO, and DELETE of INBOX in
   particular (RFC 3501 section 6.3.4).
4. The event that follows. peroxide's own create, rename or delete
   comes back as a `Labels` event 20-40 s later; `createLabel` keeps the
   existing `*mailbox` (it doesn't replace one with the same label ID),
   `updateLabel` renames only if the name changed, `deleteLabel` copes
   with a missing one. Apply the API's answer at once (as bridge does)
   so the tagged OK is followed by a LIST that shows it, and let the
   event be a no-op.
5. RENAME INBOX moves its messages to a new folder (RFC 3501 section
   6.3.5); Proton can't rename INBOX. Refuse it with NO.
6. SUBSCRIBE/UNSUBSCRIBE: peroxide has no subscriptions (LSUB lists
   every mailbox). Decide whether to accept both as no-ops, so a client
   that subscribes a folder after creating it doesn't see the CREATE
   half fail, or to keep subscriptions in the local database.
7. Client: `protonmail.Client` has only `ListLabels`; add
   `CreateLabel`, `UpdateLabel`, `DeleteLabel` (protonmail/labels.go).
8. Tests (imap): CREATE makes one POST with the name and folder type
   and lists the folder before its event; the later `Labels` create
   changes nothing; RENAME sends the color it read; DELETE of a system
   mailbox and of INBOX makes no request; a name with `/` is refused.

## 11. Determinism and protocol correctness

Found in the 2026-11-06 audit; none is from branch `labels-events`.

1. **Map order reaches clients.** FLAGS and PERMANENTFLAGS are in
   label-ID order since bc32862 (`TestStatusFlagsSorted`). Still open:
   `ListMailboxes` (imap/user.go) ranges over `u.mailboxes`, so LIST
   orders mailboxes differently each time, and `getMailbox` picks the
   first of two folders with the same name in map order (section 1
   covers the collision). Fix: LIST system mailboxes first in their
   protonmail label order (INBOX, Drafts, Sent, Starred, Archive, Spam,
   Trash, All Mail), then folders by name; look names up in the same
   order. Tests: LIST equal across repeated calls and two users built
   from the same labels in different insertion orders.
2. (done, 2e1a0f4) RFC822.SIZE is the length of the stored message
   served, no longer Proton's size.
3. (done, 1476ca8) A Refresh marks the database for a rebuild, sends BYE
   and disconnects the account's clients; the next login downloads again
   under new UIDVALIDITY values.
4. (done, 1476ca8) STATUS takes no mailbox lock and reads the stored
   counts; no sync holds it.
5. (done, 1476ca8) No API counts: every mailbox is counted locally and
   only grows as the download goes, so EXISTS never goes down.
6. (done, 3b67642) Updates reach clients in the order of the changes:
   each writer takes `sendSem` before releasing `syncSem`, and `sendOn`
   hands go-imap one update at a time, each once it was written.
7. **Untagged EXPUNGE during FETCH, STORE or SEARCH.** RFC 3501 section
   7.4.1 forbids it while a command not using UIDs runs (sequence
   numbers would shift under it), and section 5.5 asks the server to
   wait for the end of such a command. go-imap sends updates whenever
   they come, and event pages are applied at any time. Fix: hold
   EXPUNGE (not EXISTS or FETCH) for a connection while it runs a
   non-UID FETCH, STORE or SEARCH, in the `Extension` (which already
   replaces FETCH) or in NOTIFY's per-connection state
   (notify_design.md). Test: an EXPUNGE update during a FETCH reaches
   the client after its tagged OK.
8. **No UIDPLUS, no UNSEEN in SELECT.** COPY and MOVE answer without
   `COPYUID` (RFC 4315, RFC 6851 section 4.3), so clients that cache by
   UID look the copies up again; `Status` sets `UnseenSeqNum = 0 //
   TODO` (imap/mailbox.go), so SELECT has no `[UNSEEN n]`. Both need
   the UIDs to persist first (section 9.1, done in 1476ca8, so this is
   no longer blocked): a COPYUID naming UIDs the next session reassigned
   would have misled the client's cache.

## 12. Nits from the 2026-11-06 audit

Small, independent, found reviewing branch `labels-events`.

1. **One poll wakes one waiter.** After each page, `receiveEvents`
   (imap/user.go) does a non-blocking send on the unbuffered
   `u.eventSent`: only one goroutine waiting in `user.poll` (NOOP,
   APPEND) is woken, and only if it is already waiting. With two or
   more concurrent waiters (several clients NOOPing, aerc's two
   connections) the others wait for the next page, a whole poll
   interval (20-40 s) when nothing changes. Since 89f7997 a waiter also
   returns when its poll fails, but a successful one still needs
   `eventSent`. Fix: replace `eventSent` by a channel closed after each
   page and swapped for a fresh one under `u.Lock` (a broadcast); `poll`
   takes the current channel before asking for the poll. Test: three
   concurrent `poll` calls all return after one page.
2. **`More` with an unchanged ID would loop hot.** `receivePages`
   (events/events.go) asks again at once while `event.More == 1`. A page
   whose `ID` equals the `last` it was asked with, and `More` 1, would
   repeat forever with no wait. The API is a trust boundary: break when
   `event.ID == last` (log it) and wait for the next poll, as
   go-proton-api's stream skips a page whose last ID is unchanged.
   Test: a fakeSource page `{ID: last, More: 1}` is asked for once per
   poll.
3. **The event ID is written every poll.** `receiveEvents` calls
   `u.db.SetLastEventID(event.ID)` after every page, a bolt write with
   fsync, also when Proton answered "nothing new" (`ID` unchanged):
   every 20-40 s per account while a client is logged in. Fix: skip the
   write when the ID is the one last stored (keep it in `user`). Test:
   two empty pages make one write (bolt `DB.Stats().TxStats`).
4. **Locked-key log repeats.** `handleAddressEvent` (imap/addresses.go)
   runs `reportLockedKeys` on every create and update of an address, so
   any change to it (display name, signature, order) logs again "Key ...
   is new: it is unlocked when peroxide restarts" for each locked key.
   Fix: remember the fingerprints already reported in `user` and log
   each once per session. Test: two updates of an address with one new
   key log once.
5. (done, 2e1a0f4) Pass-through and fallbacks log once, when the
   message is built.
6. (done, 2e1a0f4) Attachment sizes in BODYSTRUCTURE are computed from
   the bytes served.
7. **No charset without a MIME type.** `inlineHeader` (imap/message.go)
   sets no Content-Type when `msg.MIMEType` is empty (it logs "Sending an
   inline header without its proper MIME type"), and so no charset.
   Before d6657fd BODYSTRUCTURE still claimed `charset=utf-8`; it now
   reads the part's header, so both agree on `text/plain` without a
   charset, which RFC 2045 defaults to US-ASCII though the body is
   UTF-8. Fix: default to `text/plain; charset=utf-8` in `inlineHeader`.
   Test: a message without MIMEType gets that in BODYSTRUCTURE and in
   `BODY[1.MIME]`.
8. **The events path in the docs.** aerc_implementation.md section 1
   and notify_design.md say peroxide polls `/core/v4/events`; it asks
   for `/events/{id}` under its API root (protonmail/events.go,
   `GetEvent`), `https://mail.proton.me/api/events/...` with the default
   endpoint. Bridge and go-proton-api use `/core/v4/events`. Either
   correct the docs or move peroxide to the `core/v4` path, checking it
   with a `-debug` log first.
9. (done, 97af4f7: `go fix ./...` applied, `go fix -diff ./...` prints nothing)
10. **What Proton answers for an expired event ID.** Since 5d1595f the
    receiver restarts from the latest event (and rebuilds every mailbox)
    only for a 400, 404 or 422 whose code is 2001, 2061 or 2501
    (`refused`, events/events.go), the codes Proton's web client names
    for an invalid value, ID or object; the answer for an ID Proton has
    expired was never observed. Check: with peroxide stopped, set the
    stored ID to one from weeks ago or to `AAAA` (bolt bucket `events`,
    key `last`, of `<user>.db`), run `peroxide -debug serve`, log in, and
    record the status and Code of `GET /events/...`. If it is none of
    those, add it to `refused` and to `TestRefused`; if it is a 2xx with
    `Refresh` set, nothing to do (`EventRefreshMail` is handled).
11. (done, 87d4da7, c7983f8, deaf2a4: MOVE is label then unlabel, proton-bridge's default
    `moveMessagesWithUnlabelCallOnFolders`; INBOX/Sent, All Mail and
    Starred moves refused.) Left open: the unlabel after a move into a
    folder only answers "does not exist", a 0.4 s round trip. Bridge
    skipped it in 3.24.0 (BRIDGE-451) and restored it in 3.24.1
    (BRIDGE-522) after client trouble its code doesn't describe. Revisit
    if bridge's flag (`FolderUnlabelCallDisabled`) is ever turned on by
    default.
12. **Make the SMTP domain configurable.** `listenAndServeSMTP`
    (cmd/peroxide/main.go) sets `s.Domain = "localhost"`. go-smtp uses it
    only in the greeting (`220 localhost ESMTP Service Ready`, go-smtp
    conn.go); nothing sent to Proton carries it. It matters to a client
    that checks the greeting against the host it connected to, which
    happens once peroxide listens off loopback with a certificate. Add a
    `-smtp-domain` flag, defaulting to `localhost`, or with
    `-tls-cert` the certificate's first DNS name; document it in
    docs/commands.md. Test: the greeting carries the configured name.

## 13. Make error handling congruent across the code

A sweep, when nothing else touches these files, so that every error is
handled the one way AGENTS.md asks. Found in the 2026-11-07 audit; the
files named are the ones the sweep starts from, not a complete list.

1. **`errors.As` to `errors.AsType`.** captcha/captcha.go
   (`IsHumanVerificationRequired` check) and imap/actions.go
   (`nothingToDo`) still declare a target and call `errors.As`. `go
   fix` rewrites only the `var x T; if errors.As(err, &x) {` statement
   form, so these, in expressions, stay. Rewrite each as
   `apiErr, ok := errors.AsType[*protonmail.APIError](err)`.
2. **`%v` where `%w` belongs.** About 40 `fmt.Errorf` calls format the
   cause with `%v` or `%s`, cutting the chain `errors.Is` and
   `errors.AsType` follow: auth/auth.go (every auth file error),
   protonmail/srp.go, protonmail/auth.go, protonmail/keys.go,
   smtp/smtp.go,
   config/tls.go, captcha/server.go and captcha/browser.go. So a
   `*protonmail.APIError` returned through `smtp.go`'s "cannot send
   message" can't be told from any other failure, and the SMTP reply
   can't depend on its code. Grep: `Errorf(.*%[sv]".*err)`.
3. **Comparing errors with `==`.** `err == io.EOF`
   (protonmail/messages.go, smtp/smtp.go),
   `err == ErrNotFound` (imap/database/user.go, and the imap tests),
   `err == errUsedSession || err == errExpiredSession`
   (captcha/server.go). Each works today because the error is returned
   unwrapped; `errors.Is` keeps working once one is wrapped.
4. **Messages.** Prefixes mix "cannot", "failed to" and "unable"
   ("unable read CA file" lacks its "to"). Pick "cannot <verb> <what>:
   %w", the most common form, and say what failed in the caller's
   terms, as `fmt.Errorf("open config: %w", err)` in AGENTS.md.
5. **Returned without context.** About 250 bare `return err` (and
   `return nil, err`). Most are right (the callee already says what
   failed), but those returning a database or HTTP error straight to
   go-imap reach the client as "NO <bolt or net error>" with no
   mailbox or message named: imap/mailbox.go (`fromSeqSet`,
   `fetchMessage`, `SearchMessages`) and imap/database. Wrap those
   with what was being done.
6. **Logged and dropped.** Some failures are only logged: an address or
   label event that can't be applied (imap/addresses.go,
   imap/user.go `createLabel`, `deleteLabel`) and a counts request after
   an event. List each with why it can't
   propagate, in a comment at the site, so the next reader doesn't
   "fix" it.

Tests: none of this changes behaviour but item 2, whose test is one
per changed path that a wrapped `*protonmail.APIError` is found with
`errors.AsType` by the caller (e.g. SMTP send failing with code 2028).

## 14. Upgrade to Gluon

peroxide is to move to gluon, proton-bridge's IMAP library, so that it
behaves as proton-bridge does. What gluon brings that go-imap v1 doesn't
let peroxide fix, found as they come:

1. **No per-session view of a mailbox** (2026-11-08 audit of uid-persist,
   item 6). go-imap v1 gives a backend no session state, so every
   connection reads the shared database as it is now: `FETCH 1:*` reads
   the count when it runs and can answer sequence numbers the client was
   never told of (EXISTS still on its way, a download page landing), and
   an EXPUNGE caused by another session renumbers messages in the middle
   of a command. RFC 3501 section 7.4.1 requires that a session's
   sequence numbers change only through the responses it is sent. gluon
   keeps a snapshot per session (internal/state) and applies other
   sessions' changes to it only between commands. Left as is until the
   move.

To plan with the move: the whole store changes (sqlite and gluon's
encrypted store instead of bolt), so every mailbox gets a new
UIDVALIDITY and is downloaded again; gluon needs cgo
(`mattn/go-sqlite3`), and release.sh builds with `CGO_ENABLED=0` for
every target, so the release matrix needs cgo cross-compilers.

## 15. Manual tests

Checks that need a live Proton account and a client, for behaviour the
unit tests can't reach.

1. **A rebuild from a refused event ID** (2026-11-08 audit, "Noticed").
   Shows the BYE, the disconnect, the wipe at the next login and whether
   the client reconnects by itself.
   1. Log aerc in, let the download finish, note a message's UID
      (`:pipe -m` the UID FETCH from `-debug`) and the mailbox's
      UIDVALIDITY (STATUS in the `-debug` log).
   2. Stop peroxide. Set the stored event ID to one Proton refuses,
      e.g. with a small Go program (bbolt, bucket `events`, key `last`,
      value `bogus`) on a copy, then put the copy in place.
   3. Start `peroxide -debug serve`, connect aerc.
   4. Expected: "Proton refused the events after bogus (...): starting
      again from event ..., and rebuilding every mailbox"; aerc gets
      `* BYE` and is disconnected; on reconnect the log shows "was
      marked for a rebuild: wiping it" and a new download; the new
      UIDVALIDITY is higher than the one noted.
   5. Record whether aerc reconnected by itself and how it showed the
      BYE.
2. **The message store** (section 9, 2026-11-09).
   1. Log aerc in to a fresh account copy; `-debug` shows the metadata
      download, then "Downloading N messages of ...", and in the end
      "Downloaded N messages of ..., 0 failed"; `ls ~/.local/share/peroxide/<user>/`
      has N files, and `grep -c` of a known subject in them finds none.
   2. Open a message received as PGP/MIME (signed, from another
      OpenPGP user): aerc shows the parts, and verifies the signature.
   3. Edit a draft in Proton's web client; within a poll aerc shows it
      once, with the new text (EXPUNGE then EXISTS in `-debug`).
   4. Restart peroxide with another bridge password (`peroxide auth`
      again): the log says the messages were stored under another
      bridge password, and they download again.
   5. Note how long the download of a large account takes, and whether
      any 429 shows up in `-debug` (the AIMD halving).

## 16. aerc and the download's UID order

Clients that sort by date themselves (Thunderbird, Evolution, KMail)
show the downloaded mail in order. aerc doesn't: it sorts on the server
only, with SORT (aerc worker/imap/open.go, `handleFetchDirectoryContents`:
"SORT is not supported but requested: list messages by UID"), which
neither peroxide nor gluon offers, so after the newest-first download it
lists the oldest message at the top. Fixed in aerc, not here
(aerc_implementation.md, item 5, outside this repository).
