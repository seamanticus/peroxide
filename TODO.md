# TODO

Work left for a later session, found while fixing unread counts and
latency in October 2026. Each item says what is wrong, where, what was
observed, and the steps a fix needs.

## 1. (done: folders and labels as `Folders/<path>` and `Labels/<name>` mailboxes; labels-mailboxes 3a71f0f, 6f2abc2, 2dfbdbf)

Described in docs/imap/mailboxes.md and docs/imap/changes.md. Decided
on 2026-11-09:

- Naming as proton-bridge; labels are mailboxes only, no longer
  keywords. Names come from `Name` and `ParentID`, never `Path`: Proton
  escapes `/` in `Path` as `\/` but not `\`, so `A/B` at the top and `B`
  inside `A\` share a `Path` (seen live). A `/` in a name is shown as
  U+2215; `\` as is.
- MOVE as bridge's `shouldExpungeOldLocation`; APPEND into a label
  labels a message the account has and refuses a new one (NO [ALERT]).
- A renamed parent renames its subfolders locally, then one
  `GET /labels` checks them (bridge fetches the list once per child).
- `Exclusive` changing switches the kind in place, keeping UIDs.
- A deleted mailbox sends BYE to the connections that have it selected.
- LIST order and name lookup are deterministic (section 11.1).

**Left open.**

1. (done, 2026-11-09 `-debug` run with aerc, Evolution and the web
   client) Label rename and delete applied; a parent rename made one
   `GET /labels` and its subfolder kept its UIDVALIDITY under the new
   name; aerc `+x` made one label call and no EXPUNGE, `-x` label,
   unlabel and EXPUNGE; a deleted selected folder got BYE on that
   connection only, and aerc logged in again and listed. Neither client
   drops a deleted or renamed mailbox from its tree: aerc keeps its
   directory list until restarted and re-SELECTs the deleted one
   (aerc_implementation.md section 1); Evolution re-LISTs after a
   failed STATUS and still shows it. Evolution draws `Folders` and
   `Labels` as parents; aerc's flat list shows full names.
2. (moved to section 14, item 2) go-imap's server does not close the
   connection after the BYE of a deleted mailbox.
3. (done, section 10) CREATE, RENAME and DELETE under `Folders/` and
   `Labels/`.

## 2. (done: STORE `\Deleted` sends FETCH, STORE FLAGS replaces, 1dfcddc)

## 3. (done: APPEND into every mailbox but All Mail and Starred, by import, Drafts too; a message already in the account is not imported again (by Message-Id), OK/NO [ALERT]; APPENDLIMIT 70 MiB; a195b04, bd3c765, 426b9c4, 9a24dd5, d07f346, 38cb20f, e094ad3)

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

## 10. (done: CREATE, DELETE and RENAME of `Folders/` and `Labels/` mailboxes; ca51e1c, 61a8838, e47ab12, e4921be)

Described in docs/imap/changes.md. As proton-bridge (connector.go,
`CreateMailbox`, `UpdateMailboxName`, `DeleteMailbox`), through the
same requests as go-proton-api (`POST`, `PUT`, `DELETE
/core/v4/labels`), each applied at once from Proton's answer, the
`Labels` event that follows changing nothing. Decided on 2026-11-09:
missing parent folders are created (as gluon); new labels get bridge's
`#f66`; RENAME sends the color peroxide keeps instead of reading it
again; system mailboxes, INBOX, `Folders` and `Labels` are refused.
SUBSCRIBE and UNSUBSCRIBE came after (6a65a32, 59dc0c5), kept locally
as gluon keeps them; described in docs/imap/mailboxes.md.

**Checked live** (closed 2026-11-09).

1. (done, 2026-11-09 `-debug` run with aerc, Evolution and the web
   client) `POST /core/v4/labels` answers a folder with `Type` 3, no
   `Exclusive`, and a `ParentID` for a subfolder; the `Labels` event
   after it has `Type` 1, `Exclusive` 1, and changed nothing. CREATE
   `Folders/a/b` made `a` then `b`. On a free plan the second folder
   is refused (`[2011] Folder limit reached`) and the NO says so.
   Evolution renamed a folder holding subfolders with messages, which
   followed; its RENAME of `Folders` was refused.
2. (done, same run) DELETE of a folder deletes its subfolders too
   (their events come seconds later; fixed in e47ab12, which removes
   them at once), and leaves its messages in no folder: one APPENDed
   to `Folders/a/b` kept only labels 5, 15 and 25 (All Mail and two
   categories), so it shows in All Mail only. bridge's comment says
   Proton moves them out of the folder, not where to.
3. (done, e4921be, checked live 2026-11-09) RENAME of a subfolder to the
   top of `Folders` sends `ParentID` 0, as Proton's web client does
   (WebClients `ROOT_FOLDER`), which go-proton-api can't send. Proton
   accepted it: its answer has `Type` 3 and no `ParentID`, and `Folders/
   t/r` became `Folders/r`, with and without messages, and back again.
4. Client side, not peroxide's: aerc's `:rename` and its refresh after
   `:mkdir`/`:rmdir` are planned in aerc_implementation.md section 10;
   Evolution, which isn't maintained against, doesn't list again after
   a folder moves elsewhere.

## 11. Determinism and protocol correctness

Found in the 2026-11-06 audit; none is from branch `labels-events`.

1. (done, labels-mailboxes 3a71f0f) LIST and name lookup follow a fixed
   order: system mailboxes, then `Folders` and the folders by name, then
   `Labels` and the labels by name (`TestListOrderAndHierarchy`, both
   insertion orders).
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
2. **BYE without closing the connection** (2026-11-09, TODO 1). When a
   selected mailbox is deleted (`user.deleteLabel`), peroxide's BYE
   reaches only the connections that have it selected, through go-imap's
   `listenUpdates`, but go-imap's server doesn't close them after it.
   aerc closes the connection itself on BYE (go-imap client,
   `client.go`) and logs in again at once, so nothing shows; a client
   that ignores BYE keeps a connection whose commands fail. Tracking
   selections in `Extension` (SELECT, EXAMINE, CLOSE, UNSELECT, the
   connection ending, which go-imap gives no hook for) would not remove
   the data race `listenUpdates` already has reading each connection's
   selected mailbox, and go-imap gives no signal for when the BYE is
   written. gluon checks on each session's own goroutine, before every
   command, that its state is still valid, and answers `* BYE IMAP
   session state is inconsistent` then closes (internal/session,
   `session.go`). In go-imap that needs every command handler wrapped.
   Left as is: documented in docs/imap/mailboxes.md.

Whether to move at all is not decided. Against: the store and download
would be redone (below), gluon's go.mod needs cgo (mattn/go-sqlite3),
and it is built for proton-bridge. For: the items above, which go-imap
v1 gives the backend no way to fix.

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

## 17. (done: EXPUNGE deletes for good only from Trash, Spam and Drafts, moves to Trash from other folders; a61aff7)

EXPUNGE from a folder unlabelled the messages, as proton-bridge does,
leaving them in All Mail only (seen 2026-11-09: 29 messages with labels
`[5, 15, 24]`, every folder empty). Decided 2026-11-09, following
Proton's web client: Trash, Spam and Drafts delete for good (Spam is
new); any other folder moves to Trash and answers OK [ALERT] saying so;
labels and Starred unlabel; All Mail refuses. aerc's `:delete` now does
what the web client's Delete does. Described in
docs/imap/changes.md#expunge.

## 18. (done: `serve` is the only server command; the `imap` and `smtp` commands are removed; 7a0dca8)

Run as two processes, `peroxide imap` and `peroxide smtp` each kept
their own auth.Manager and refreshed the same account's session; Proton
refresh tokens are single use, so the slower one got `[10013] Invalid
refresh token`, then CAPTCHA (`9001`) on its re-login, and answered NO
until `peroxide auth` was run again (seen 2026-11-09). `serve` runs both
servers on one auth.Manager. Decided 2026-11-10: peroxide is a desktop
server, `serve` is all it needs. Described in docs/commands.md.

**Then** (2026-11-10), every way two refreshes of one token could meet:

- within `serve`: logins of one account, and requests meeting an
  expired session, each refreshed on their own (b073584: one at a time);
- `auth.json` written in place, readable half written (8f71c1e: temp
  file and rename);
- `download-mailbox` beside `serve` (4cdce52: `serve.lock`, refused
  before logging in);
- `sendmail` beside `serve` (43631a2: through `serve`'s SMTP, TLS
  certificate pinned);
- anything else that spends the token: on 10013, the token auth.json
  holds is tried before logging in (9f63ab5);
- two writers of auth.json at once, even of different accounts within
  one `serve`, each reading, changing and rewriting it, losing the
  other's change (`auth.lock`, held across the read and the write).

## 19. SMTP can't send a PGP/MIME message the client encrypted

**Problem.** A message the client encrypted itself (aerc's encrypt on
send: `Content-Type: multipart/encrypted;
protocol="application/pgp-encrypted"`) fails with `554 cannot upload
attachment: [2001] Invalid input`. Seen 2026-11-09 09:29 and 09:30
(build_release/smtp.log, "SMTP encrypted"). SendMail (smtp/smtp.go,
hydroxide's send path) walks the MIME parts with go-message's
mail.Reader: it finds no text body to use, takes the
`application/octet-stream` part as an attachment with an empty filename
(log: `uploading message attachment ""`), and Proton refuses the
upload. Not caused by branch append-import, which changed only what
SendMail returns.

**What a fix needs to decide:**

- Such a message is already end-to-end encrypted to the recipient's key
  by the client: Proton must send it as is, not re-encrypt it. The
  Proton package type for that is PGP/MIME (`MessagePackagePGPMIME`,
  16) with the client's ciphertext as the body, or the message sent
  as an attachment-less MIME body of type multipart/encrypted. Check
  how proton-bridge's SMTP handles an incoming multipart/encrypted
  message (internal/services/smtp, message/parser) before choosing.
- The copy kept in Sent: Proton stores what it sends; the client's
  ciphertext is encrypted to the recipient, maybe not to the sender, so
  the sent copy may be unreadable to the account. aerc's
  `pgp-self-encrypt` (or equivalent) adds the sender's key.
- multipart/signed (sign only) goes through today? Check: the signed
  part is an inline body and the signature an attachment
  (signature.asc), which may reach the recipient as a separate
  attachment, breaking the signature.
- A test in smtp/smtp_test.go with a multipart/encrypted message.

## 20. Building a large message peaks at about 14x its size

**Problem.** The first FETCH of a 28.7 MB message (a 20 MiB `.log`
attachment, imported into Archive) made peroxide download, decrypt and
build it: gctrace showed the Go heap at 105 -> 191 -> 308 -> 370 MB
(before.log, after.log in the append-import measurements, 2026-11-09
14:05 and 14:21), process RSS 414-430 MiB, against 13 MiB idle. The
download's memBudget (64 MiB, imap/limiter.go) bounds how many builds
run at once by Proton's sizes, not what one build allocates. Not caused
by branch append-import.

**To do:** a benchmark of buildMessage (imap/build.go) for a message
with a 20 MiB attachment, as BenchmarkImportMessage20MiB does for
imports, then the same treatment as protonmail/encrypt_rfc822.go got
(408c39c): stream decryption into the output, size buffers up front.
The attachment is downloaded whole (GET /attachments/{id}), decrypted,
base64-encoded into the literal, which is sealed into the store
(imap/database/store.go): each step may hold a full copy.

**Also, on the APPEND side** (protonmail/encrypt_rfc822.go): a message
encrypted whole (a signed or encrypted part, or two text parts) first
has a per-part attempt, whose output buffer, grown up front to the
literal plus an eighth, is dropped when the attempt fails: about 30 MB
wasted for a 28.7 MB message. BenchmarkImportMessage20MiBText measured
3.55 copies at da00766 (102 MB per import), live RSS 177 MiB. A walk
of the MIME structure before allocating (the same checks encryptEntity
makes: multipart/encrypted or /signed, a second text part, an
unparseable Content-Disposition) would pick the path first and save
that buffer: about 2.4 copies. Measured series, 2026-11-09: 780cc70
9.01 copies / 315 MiB, 408c39c 1.87 per part, 5f5ce61 5.53 whole /
236 MiB, da00766 3.55 whole / 177 MiB.
