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

1. `applyUpdate`: skip mailboxes not synced yet (`mbox.initialized`
   false) instead of looking the message up, which also drops the
   warning; they get their counts from the API until synced.
2. Decide whether All Mail is shown: always (as now), or behind an
   option like bridge's `ShowAllMail`, so users who don't want the
   duplicates don't get them. Same question for Almost All Mail.
3. If shown, decide how it syncs: on first use as now, or in the
   background after login, so a search over it doesn't wait for a full
   listing.
4. Tests: a flag change on a message whose All Mail isn't synced logs
   nothing and sends no FETCH for All Mail.

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
   `Exclusive` 1 (section 1, 2026-11-04). Check with a `-debug-raw` run
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
