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

1. Write a client for Proton's import API, as go-proton-api's
   `ImportMessages` does (peroxide's own, `protonmail.Client.Import`,
   was removed in b170307 and can be recovered from its parent), and
   use it for APPEND with the metadata above.
2. Refuse All Mail (and Starred, a flag here) with NO, as bridge does.
3. Decide on the SMTP duplicate check: peroxide's SMTP server sends
   through a Proton draft (`CreateDraftMessage` then `SendMessage`,
   smtp/smtp.go), which Proton files in Sent, so a client that also
   APPENDs to Sent (aerc `copy-to`) would create a second copy.
4. Tests (imap): the label IDs and flags sent for INBOX, Sent, a custom
   folder and Drafts; All Mail refused; a literal over APPENDLIMIT gets
   TOOBIG before any request. Since a882fe3 a literal over
   `imap.MaxLiteralSize` (= APPENDLIMIT) is refused by go-imap's reader
   with BAD before it is read; one within it but whose encrypted import
   exceeds 50 MB still needs NO [TOOBIG] from CreateMessage.

## 4. (done: `-debug` is the raw mode, to one stream; described in docs/debug.md)

Left open: the request line logs `req.URL.RequestURI()`
(protonmail/protonmail.go, `newRequest`) unredacted, so query values
reach the log: `Email` of public-key lookups (protonmail/keys.go),
`Address` of `/messages/count`, and `ListMessages` filters. Message and
label IDs are in paths. None is a credential; docs/debug.md says so.
If logs are ever to be shared without editing, redact query values
whose key is `Email` or `Address`, as `redactedJSONFields` does for
bodies. Test: a `GetPublicKeys` request logs `Email=<removed>`.

## 5. (done: import-messages and export-messages removed; clients use APPEND and FETCH)

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

## 8. The All Mail Problem

**What it is.** Proton keeps every message in the system label All Mail
(`"5"`, plus `"15"` Almost All Mail, which excludes Spam and Trash), and
peroxide shows it as the mailbox `All Mail` (`\All`, imap/user.go,
`systemMailboxes`). A client sees every message twice or more: once in
its folder, once in All Mail. Some want that, to search everything in one
place (people, and LLM tools reading a mailbox over IMAP); others don't.

**Observed** (2026-11-05 `-debug-raw` log, line 4504):

    cannot get sequence number of message kPAWhk... in All Mail:
    message not found in local database

logged by `user.applyUpdate` (imap/actions.go) for a `STORE +FLAGS
(\Seen)` in INBOX. The STORE succeeded and the client got its FETCH.

**Cause.** A mailbox is synced into the local database only when a client
first opens it (`mailbox.init`, from SELECT, FETCH, SEARCH...); STATUS
uses the API's counts until then. aerc STATUSes All Mail but never
selects it, so its bucket stays empty, while each message still lists
`"5"` in `LabelIDs`. `applyUpdate` walks every label of the message,
finds the All Mail mailbox, and doesn't find the message in it. Harmless
today (an unsynced mailbox needs no FETCH), but it is logged for every
flag change of every message, and the same happens for any folder not
opened yet, Starred included.

**Also.**

- Syncing All Mail means listing every message of the account
  (`mailbox.sync`, 150 per page, from 0): slow for a large account, and
  it stores each message's metadata again under another bucket.
- proton-bridge has a setting for it, `ShowAllMail`, on by default
  (internal/vault/types_settings.go): off, All Mail is hidden from LIST
  (`Connector.GetMailboxVisibility`, connector.go). It also refuses APPEND
  and moves into All Mail (`isAllMailOrScheduled`, helpers.go).
- Almost All Mail (`"15"`) and the categories (`"24"` Default and the
  others, seen in every message's `LabelIDs` in the log) have no mailbox
  in peroxide.

**Steps.**

1. (done, superseded by 1476ca8) `applyUpdate` skipped mailboxes not
   synced yet. Every mailbox is now filled by the account download, so
   none is skipped.
2. (done) All Mail is shown unless `serve -show-all-mail=false`, as
   bridge's `ShowAllMail` (on by default): off, it is left out of LIST
   only (`user.ListMailboxes`), and SELECT/STATUS still open it by name,
   as gluon does. Open: Almost All Mail (`"15"`) has no mailbox, and
   bridge shows none either (`WantLabel`); add one only if asked.
3. (done, 1476ca8) One background download of the whole account after
   login, newest first, as bridge's sync (section 9.1), whatever
   `-show-all-mail` says: like `ShowAllMail`, it only changes LIST.
   `syncSem` is held per page; events interleave (decided 2026-11-08).
4. (done) Tests: `TestStoreTellsEveryMailbox` (was
   `TestStoreSkipsUnsyncedMailbox`) and `TestListShowAllMail`.

## 9. Fetch full messages in the background (BODYSTRUCTURE cost)

**Problem.** Since commit f82e989, BODYSTRUCTURE and every body section
need the full message (`GET /messages/{id}`): the local database has
neither the body nor its MIME type, and only the decrypted body says
whether a message is passed through as PGP/MIME. aerc fetches
BODYSTRUCTURE for every message of its list (`UID FETCH 1:3
(BODYSTRUCTURE ENVELOPE INTERNALDATE FLAGS UID RFC822.SIZE
BODY.PEEK[HEADER])`), so opening a folder costs one request per message
not in the 32-message memory cache (`messageCache`, imap/cache.go). The
2026-11-05 log shows 3 requests for `1:3` and 2 for `4:5`, each 0-1 s.
A folder of 500 messages opened for the first time in a session waits
for 500 sequential requests, and again after every restart: the cache
is in memory only.

**More found in the 2026-11-06 audit.**

- `BODY.PEEK[HEADER]` and `HEADER.FIELDS` cost a request too: since
  f82e989 `fetchBodySection` gets the full message for every section,
  as the top-level Content-Type depends on pass-through. Before, a
  header fetch of a message without attachments needed none. So does
  every section of a message that isn't a PGP message at all.
- The cache holds 32 messages, fewer than one aerc list batch (the
  default `aerc` window is 50 and more on a tall terminal): listing
  evicts the messages just listed, and opening one downloads it again.
- Each item of one FETCH decrypts again: `passThrough` (msg.Read, the
  session key; since d6657fd also the key packets, and every account
  key parsed from armor, `accountKeys`), then BODYSTRUCTURE decrypts and
  quoted-printable-encodes the whole body to give its size and line
  count (`encodedInlinePart`), then `BODY[]` decrypts it once more.
  `BODYSTRUCTURE BODY.PEEK[]` of one message is three decryptions.
- What would make header fetches and BODYSTRUCTURE DB-only again,
  without storing bodies: on first full fetch, store in the message's
  bolt record the pass-through decision, the inline part's header
  (type, encoding: it differs for the armored fallback), its encoded
  size and line count, and per attachment whether it is decryptable.
  That is step 1's second option, narrowed; it must be invalidated as
  step 5 says, and cleared when keys change (a restart unlocking a new
  key turns an armored fallback into a decrypted body).
- Attachment `Size` in BODYSTRUCTURE is the decrypted size from the
  API, not the base64 size sent (RFC 3501 section 7.4.2, size "in its
  transfer encoding"); the inline part does it right. Fix with step 1.

**How proton-bridge does it** (internal/services/syncservice): one sync
after login, as a pipeline of four stages, persisted and resumable:

1. Metadata (stage_metadata.go): `GetMessageMetadataPage` 128 per page
   (`MetadataPageSize`), newest first (`Desc: true`, then `EndID` = last
   ID of the previous page, which comes back as the first result and is
   skipped). Batches of at most 64 messages (`MetadataMaxMessages`) and
   at most `DownloadRequestMem` (40-128 MB, from `MaxSyncMemory`, 2 GB by
   default) of message and attachment sizes.
2. Download (stage_download.go): `GetMessage` for each message, then
   every attachment, in parallel: up to `MaxParallelDownloads` (32, "the
   maximum recommended by the API team", doubled to 64 in
   `NewDownloadStage`). Each request retries with exponential cool-down
   on 429 and 5xx (`network.ExpCoolDown`). Parallelism adapts per chunk
   (`DefaultDownloadRateModifier`): note that its test
   (`TestAutoDownloadScale_429or500x`) expects it to fall to 2 after a
   chunk without 429 and to double after one with a 429, which looks
   inverted; don't copy it as is.
3. Build (stage_build.go): decrypt and build the RFC 822 message
   (`DecryptAndBuildRFC822Into`, `IgnoreDecryptionErrors`: an
   undecryptable body becomes the PGP/MIME fallback peroxide now uses),
   parse its structure (`imap.NewParsedMessage`).
4. Apply (stage_apply.go): store the literal and the parsed structure in
   gluon's on-disk store, so later FETCHes make no request.

Progress is saved (`syncservice.Status`: `LastSyncedMessageID`,
`NumSyncedMessages`, `FailedMessages`): a sync stopped halfway resumes,
and messages that failed are retried later. `StartSyncEventID` is the
event ID when the sync began: events received meanwhile go to a reduced
handler (messages only, `newSyncEventHandler`, service_sync_events.go),
and a failed sync rewinds the events to that ID.

**What peroxide would need.**

1. Storage, i.e. option (b) first: a bolt bucket keyed by message ID
   with what FETCH needs without a request. Either the API's full
   message as received (body still PGP-encrypted to the account's keys,
   attachments' key packets, names, MIME types, sizes), which is no less
   safe at rest than the metadata already stored in clear (subjects,
   addresses); or the computed BODYSTRUCTURE plus the pass-through flag,
   which needs no decryption per FETCH but stores attachment names in
   clear. Decryption keys are never stored.
2. A downloader per user, started by the first `mailbox.init` of a
   session (not at login: no requests while no client looks), stopped
   with the last logout (`u.done`), as the events poll is. Newest first,
   the selected mailbox before the others.
3. Bounded parallelism (start at 4, at most 32 per bridge's figure),
   halved on 429 or 5xx with exponential back-off, raised slowly while
   requests succeed. No attachments: BODYSTRUCTURE needs only the
   message; attachments are fetched when a client asks for them.
4. Interactive FETCHes go first: a FETCH needing a message the
   downloader hasn't reached fetches it at once (as now) and stores it,
   and the downloader skips it.
5. Invalidation: `EventUpdate` (a draft edited) and deletes remove the
   stored entry, and a Refresh event clears all of it, as already done
   for the memory cache (`receiveEvents`, imap/user.go; `applyDelete`,
   imap/actions.go).
6. Resume: the stored entries are the progress, so a restart continues
   where the last session stopped. This needs `mailbox.sync` to stop
   resetting the mailbox on the first open of every session ("TODO:
   sync only the first time", "TODO: don't do this without incrementing
   UIDVALIDITY", imap/mailbox.go), or the downloader would start over
   each session.
7. Tests: a stored message makes BODYSTRUCTURE and BODY[] make no
   request; an edited draft is fetched again; on 429 the downloader
   backs off and the parallelism drops (testing/synctest); a FETCH of a
   message being downloaded makes one request, not two.
8. ENVELOPE from the original header (with the storage of step 1).
   Since 49b614e `messageHeader` starts from the header Proton
   received (`Message.Header`, sent only by `GET /messages/{id}`: the
   metadata pages have neither it nor `ParsedHeaders`), so BODY[HEADER]
   keeps References, In-Reply-To and the sent Date. `fetchEnvelope`
   still reads the metadata only, as aerc asks ENVELOPE of every
   listed message and a full message each would cost a request each:
   - In-Reply-To is always NIL, so clients that thread by ENVELOPE (as
     aerc does without the THREAD extension) can't link replies.
   - Date is Proton's receive time (`Time`), not the header's Date.
   - Sender is always From: a real `Sender` header (mailing lists,
     sending on behalf) is lost.
   - Message-Id is `<ExternalID>`; the original header's value could
     differ in case or form.
   proton-bridge builds ENVELOPE from the full header (gluon
   imap/envelope.go, at sync, stored with the message), defaulting
   Sender and Reply-To to From. Once full messages are stored, build it
   from `messageHeader(full)` the same way: parse Date, In-Reply-To,
   Message-Id and the address fields from it, keeping the metadata's
   subject and addresses where the header has none, and fall back to the
   metadata while a message isn't stored yet.
   Tests: a stored message's ENVELOPE has the header's In-Reply-To,
   Date and Sender; an unstored one the metadata's, without a request.

### 9.1 Keep UIDs across sessions (option b)

**Done (1476ca8), as proton-bridge v3 does it** (decisions of
2026-11-08): one background download of the whole account at login,
newest first (`POST /mail/v4/messages` `{Desc, EndID}`, 128 per page,
cursor stored with each page, so it resumes), so synced mail gets UIDs
in reverse arrival order as with bridge, left to clients to sort;
`syncSem` per page with events applied between pages, creates
idempotent; UIDVALIDITY per mailbox from gluon's epoch, refused before
2023-02-01; a schema version that wipes older databases; counts kept;
a UID index per mailbox. Repairs are rebuilds from a wiped database,
never prunes: a Refresh, or a message event that fails, stores no event
ID, marks the database (`meta/rebuild`, bridge's `ShouldResync`), sends
BYE and disconnects the account's clients; the next login wipes and
downloads again. `peroxide download-mailbox [-discard-local-copy]` runs
the download from the command line. docs/imap/sync.md has the details.
Still open: the bodies (section 9 steps 1-4, 7, 8). What follows is the
plan as written before, kept for the record.

**Problem.** `mailbox.init` syncs a mailbox on its first use in every
session (FETCH, SEARCH, STORE, COPY, MOVE, EXPUNGE, APPEND; not SELECT
or STATUS, which use the API's counts until then), and `mailbox.sync`
(imap/mailbox.go) starts with
`mbox.db.Reset()`: it deletes the mailbox's bucket and recreates it,
so its bolt sequence restarts and every message gets a new UID from 1.
UIDVALIDITY stays 1 (`Status`, `StatusUidValidity`). RFC 3501 section
2.3.1.1 forbids it: a UID may never name another message under the same
UIDVALIDITY.

**Observed** (2026-11-05 `-debug-raw` log, bob, INBOX):

| Message | UID 21:50 | UID 21:56 |
|---|---|---|
| LOGOUT TEST (arrived 21:52:59, while logged out) | - | 1 |
| Peroxide non-encrypted test | 1 | 2 |
| PEROXIDE ENCRYPTED TEST 3 | 2 | 3 |
| PEROXIDE ENCRYPT TEST 4 | 3 | 4 |

The message got UID 1, below the older ones, so aerc (which orders by
UID) showed it last. The cause was a race: its replayed create event was
applied while the 21:56 sync had emptied INBOX. Commit c3dc632 serializes
syncs, event pages and actions (`user.syncSem`), so the message now gets
the next UID after the sync; but the renumbering of the others remains.

**Why it matters.**

- Clients that cache by UID see the wrong message: aerc's header cache
  (`cache-headers`, worker/imap/cache.go) keys entries by mailbox,
  UIDVALIDITY and UID (`header.<mailbox>.<uidvalidity>.<uid>`), so with
  it on, a renumbered message shows another's headers. Any client with
  an offline store keyed by UID is exposed the same way.
- A client that stays connected across a Refresh event keeps UIDs that
  `reset` then reuses (NOTIFY design question 3).
- Every session lists every opened mailbox from Proton again: one
  request per 150 messages, before the first command answers.
- It blocks section 9 step 6: a background downloader would start over
  each session.

**How proton-bridge does it.** gluon keeps each mailbox's UIDs and its
UIDVALIDITY in its database across sessions. A sync adds only messages
it doesn't know and removes those gone; UIDVALIDITY is bumped only when
the store must be rebuilt (`NewUIDValidityBumpedStateUpdate`, which also
marks connected clients' state invalid so they get BYE and reconnect).

**Steps.**

1. Store per mailbox, in its bucket or the counts bucket: UIDVALIDITY
   (start at a value derived from the time, as servers usually do, so
   it never repeats after a database is deleted), and whether the
   mailbox has ever been synced. `Status` and SELECT report the stored
   UIDVALIDITY.
2. `mailbox.sync`: no `Reset`. List the mailbox from Proton, add the
   messages missing from the bucket (new UIDs, above the bolt sequence),
   remove the entries no longer listed, and leave the others with their
   UIDs. Messages missed while logged out arrive anyway as replayed
   events (resume, commit dafd522); the listing then repairs what an
   event couldn't (an event lost to a Refresh, a database from an older
   peroxide).
3. Order: Proton lists by ID (`Sort: "ID"`, ascending), which is roughly
   arrival order; messages added during a sync get UIDs in that order.
   A message older than ones already stored (moved in from another
   folder) gets a higher UID, which is correct IMAP: UIDs order arrival
   in the mailbox, not dates.
4. `reset` (Refresh event) and any other case where UIDs can't be kept:
   clear the bucket and increment UIDVALIDITY; tell connected clients
   (a `* OK [UIDVALIDITY n]` is not enough for a selected mailbox: send
   BYE, as gluon does, so they resync).
5. Sync once per database, not per session: `initialized` comes from the
   stored flag, and later sessions rely on events. Keep a way to force a
   full listing (a new `peroxide -resync` flag, or on a Refresh event).
6. Background sync of All Mail (section 8 step 3), when shown: started
   after login, owned by the user and stopped at logout, resuming from
   the stored progress. It takes `syncSem` one page at a time, and
   message events for All Mail that arrive meanwhile are applied to it
   only after the listing ends, so they still get the highest UIDs. A
   client opening All Mail early waits for it rather than starting a
   second sync. Its listing carries each message's `LabelIDs`, so it can
   fill the other mailboxes' buckets too, as bridge's single pass does;
   decide then whether it replaces their per-mailbox syncs.
7. Tests (imap, database): a second session keeps the UIDs and
   UIDVALIDITY of the first; a message added while logged out gets the
   next UID; one removed is expunged; a Refresh changes UIDVALIDITY and
   restarts UIDs; STATUS UIDVALIDITY matches SELECT; an event during the
   background sync waits only one page and gets a UID above the listed
   ones (`testing/synctest`); logout stops the sync.

**Found in the 2026-11-06 audit; both get worse once syncs stop.**

- A message event that fails to apply is lost for good. In
  `receiveEvents` (imap/user.go) a create, update or delete that fails
  in the database (`cannot handle create event ...`) is only logged,
  and the page's ID is stored anyway (`SetLastEventID` after the page),
  so the next session resumes after it. Today the resync of every
  mailbox at its first use each session repairs it. With step 5 it
  stays wrong until a full listing. Step 7: count the messages a page
  failed to apply and, if any, store the page ID but mark the mailboxes
  they belong to (by label ID) as needing a listing (the stored "synced"
  flag of step 1 cleared), so their next use relists them; test that
  a failed create makes the next session list INBOX.
- `user.syncSem` (commit c3dc632) is one slot for the whole account,
  held for a whole `mailbox.sync`: one request per 150 messages, so
  minutes for a first sync of All Mail. Meanwhile every event page
  waits, and so does every STORE, COPY, MOVE and EXPUNGE, and even the
  `\Seen` that a FETCH of `BODY[]` sets (`setsSeen` goes through
  `applyAction`), in every mailbox of the account. It was chosen for
  correctness: without it a page applied while a sync had emptied
  INBOX gave its message UID 1. With steps 2 and 5, syncs no longer
  empty buckets, so step 8: make the semaphore per mailbox (an event
  page then takes those of the mailboxes it touches, in label-ID order
  to avoid deadlocks), or hold it per page of the listing rather than
  for the whole sync; test that a STORE in INBOX answers while All Mail
  syncs (testing/synctest, the listing blocked).

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

1. **Map order reaches clients.** `ListMailboxes` (imap/user.go) ranges
   over `u.mailboxes`, so LIST orders mailboxes differently each time;
   `Status` (imap/mailbox.go) ranges over `u.flags`, so FLAGS and
   PERMANENTFLAGS of SELECT reorder labels. AGENTS.md wants
   `slices.Sorted(maps.Keys(m))`. Fix: LIST in a fixed order, system
   mailboxes first in their protonmail label order (INBOX, Drafts,
   Sent, Starred, Archive, Spam, Trash, All Mail) then folders by name;
   flags sorted by label ID behind `\Seen \Deleted`. `getMailbox` also
   picks the first of two folders with the same name in map order
   (section 1 covers the collision). Tests: LIST and FLAGS equal across
   repeated calls and two users built from the same labels in different
   insertion orders.
2. **RFC822.SIZE is Proton's size, not the size served.** `fetchMessage`
   answers `msg.Size` (the API's, of the message as Proton stores it),
   SEARCH LARGER/SMALLER uses it too, but `BODY[]` is what peroxide
   builds: another header, quoted-printable, base64 attachments, or
   PGP/MIME. RFC 3501 section 2.3.4 requires the size of what FETCH
   `BODY[]` returns; clients size progress bars and partial fetches by
   it. Fix: compute it as `len(BODY[])` the first time the full message
   is built and store it in the message's bolt record with the facts of
   section 9 ("more found"); until then answer the API size (no request
   per message in a list fetch). Deterministic as long as the built
   message is: multipart boundaries already derive from the message ID
   (`messageHeader`, sha1), passThroughBoundary too; keep it so and test
   that two builds of one message give equal bytes and RFC822.SIZE.
3. (done, 1476ca8) **A Refresh rebuilds mailboxes under clients' feet.**
   A Refresh now marks the database for a rebuild, sends BYE and
   disconnects the account's clients, as proton-bridge removes its IMAP
   user; the next login wipes and downloads again with new UIDVALIDITY
   values, so no client keeps stale sequence numbers. What was written:
   On
   `EventRefreshMail`, `receiveEvents` resets every mailbox, then
   `initMailboxes` replaces `u.mailboxes` with new `*mailbox` values,
   making requests (ListLabels) while holding `u.Lock` (every
   `getMailbox`, STATUS and LIST waits on the network). Clients that
   had a mailbox selected keep the old value, whose `db` bucket was
   reset: their sequence numbers no longer match anything and no
   EXPUNGE was sent. Fix: keep the `*mailbox` values (reset them in
   place, as now), fetch the labels before taking `u.Lock`, then apply
   them as `handleLabelEvent` does (create, rename, delete) rather than
   rebuilding. Then tell each client that has a mailbox selected that
   its contents changed: go-imap v1.2.1 has no update for that, so send
   an `ExpungeUpdate` for every message, from the highest sequence
   number down, before the reset, and let the new EXISTS follow.
   Section 9.1's UIDVALIDITY bump
   on Refresh then tells UID-caching clients too. Test: a selected
   mailbox gets the expunges and its new EXISTS after a Refresh, and
   `getMailbox` doesn't wait on ListLabels.
4. (done, 1476ca8: no sync holds the mailbox lock; STATUS takes none and
   reads the stored counts) **STATUS waits for a sync.** `Status` takes `mbox.Lock` for the whole
   count, and `init` holds that lock (and `syncSem`) for the whole sync:
   a STATUS of a mailbox being synced (aerc's check-mail) waits minutes,
   though `apiCountsLock` exists so counts don't need the mailbox lock.
   Fix: read `initialized` under `mbox.Lock` and release it before
   counting; the bolt `Count` is its own transaction, and an unsynced
   mailbox answers `apiCounts()`. A sync in progress then reports API
   counts until it ends. Test (testing/synctest): STATUS answers while
   `sync` blocks on its listing.
5. (done, 1476ca8: no API counts; every mailbox is counted locally and
   only grows as the download goes) **SELECT of an unsynced mailbox can make EXISTS go down.** SELECT
   calls `init` first, so SELECT itself is exact; but STATUS (and the
   EXISTS go-imap derives from a `MailboxUpdate` for a mailbox not yet
   synced) uses `apiCounts()`, then the first event sends EXISTS with
   the local sequence number of a database that only holds what events
   added. A client that STATUSed 40 then gets `* 3 EXISTS`, which RFC
   3501 section 7.3.1 forbids below the known count without EXPUNGE.
   Fix: send no `MailboxUpdate` for an uninitialized mailbox (nobody
   can have it selected, as SELECT initializes it), and keep only the
   API counts current (`fetchCounts`, already done). Test: a create
   event for an unsynced mailbox sends no update; STATUS stays on API
   counts until SELECT.
6. **go-imap does not keep the order of updates.** Its update loop
   (go-imap server.go, `listenUpdates`) starts one goroutine per update
   and connection and takes the next update at once, so two updates for
   one connection race to its response channel. `sendUpdates`
   (imap/actions.go) queues all of an action's updates before waiting
   for any, and `receiveEvents` (imap/user.go) all of a page's. For
   EXPUNGE the order is the meaning: messages 1 and 3 removed are sent
   as `EXPUNGE 1` then `EXPUNGE 2` (each renumbers what follows, RFC
   3501 section 7.4.1); reversed, the client removes 2 then 1, the
   wrong message, and its view is off until it reselects. EXISTS before
   FETCH, and FETCH FLAGS after the EXPUNGE that renumbered it, race the
   same way. Fix: in both places wait for each update's `Done()` before
   queueing the next (a few ms per update on a local client; one
   connection blocked on writing then delays the next update for all, as
   it already delays `Done`). Test: an action expunging seq 1 and 3 hands
   go-imap the second only after the first is done (a fake updates
   channel that does not close Done until told).
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
   the UIDs to persist first (section 9.1): a COPYUID naming UIDs the
   next session reassigns would mislead the client's cache.

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
5. **Pass-through and fallbacks log at every FETCH.**
   `passThroughBodyStructure` (imap/passthrough.go) logs "Message ... is
   encrypted to a key Proton doesn't have: passed through as PGP/MIME"
   every time BODYSTRUCTURE is fetched, so every list fetch of a folder
   with such mail repeats it; `inlinePart` ("served encrypted") and
   `attachmentBody` ("Attachment ... served encrypted") likewise. Fix:
   log once per message per session (a set in `user`, or log only when
   the full message is first fetched, in `fullMessage`).
6. **Attachment size in BODYSTRUCTURE** is the API's decrypted size, not
   the size of the base64 sent (RFC 3501 section 7.4.2); for an armored
   fallback (`<name>.pgp`) it is further off, as armor and key packets
   are added. Recorded under section 9 ("more found") with its fix.
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
9. (done: `go fix ./...` applied, `go fix -diff ./...` prints nothing)
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
11. (done: MOVE is label then unlabel, proton-bridge's default
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
   smtp/smtp.go, the draft path of imap/message.go (`createMessage`),
   config/tls.go, captcha/server.go and captcha/browser.go. So a
   `*protonmail.APIError` returned through `smtp.go`'s "cannot send
   message" can't be told from any other failure, and the SMTP reply
   can't depend on its code. Grep: `Errorf(.*%[sv]".*err)`.
3. **Comparing errors with `==`.** `err == io.EOF`
   (protonmail/messages.go, smtp/smtp.go, imap/message.go),
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
