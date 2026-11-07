# The local database, events and UIDs

## The local database

Each account has a bolt database, `~/.config/peroxide/<username>.db`
(package `imap/database`). It holds:

| Bucket | Content |
|---|---|
| `messages` | Each message's metadata as Proton lists it, JSON, keyed by Proton's message ID: subject, addresses, date, size, flags, label IDs, attachment list. No body |
| `mailboxes/<label ID>` | One bucket per mailbox: its UIDVALIDITY, and its messages by UID and by message ID |
| `counts` | Total and unread per mailbox, kept as messages change, so STATUS doesn't count |
| `events` | `last`: the ID of the last Proton event applied |
| `meta` | The schema version, the last UIDVALIDITY given, where the download stands, and whether a rebuild was asked for |

It is not encrypted. See [../accounts.md](../accounts.md#files-on-disk).

A database left by an older peroxide (another schema version) is
wiped when opened, and downloaded again: it is only a copy of what
Proton has.

## The download

At login, peroxide **downloads the metadata of every message of the
account** in the background, as proton-bridge syncs, and only once per
database: later logins rely on events.

1. It lists the account **newest first**, 128 messages per request,
   every label at once: `POST /mail/v4/messages` with
   `X-HTTP-Method-Override: GET` and `{Desc: 1, PageSize: 128, Sort:
   "ID"}`, then `EndID` = the oldest message of the page before (which
   Proton lists again first, and peroxide skips).
2. Each message is filed in the mailbox of each of its labels, with the
   next UID there. So **the newest message has UID 1**: UIDs of
   downloaded mail run in reverse arrival order, as with proton-bridge.
   Mail that arrives later counts up from there. Clients that sort by
   date themselves (Thunderbird, Evolution, KMail) show the mail in
   order. aerc sorts only on the server, with the SORT extension, which
   peroxide (like proton-bridge) doesn't offer: aerc lists in UID order,
   so downloaded mail appears oldest at the top, newest at the bottom,
   above anything that arrived after it. aerc's `:sort` and `sort`
   option need SORT; a client-side sort is a change to make in aerc.
3. Each page and the point reached are stored in one transaction, so a
   download stopped at any point (peroxide stopped, the last client
   logged out) resumes from there at the next login.
4. A failed request is retried after 20 s, as proton-bridge does.

Clients can use their mailboxes while they fill: each page sends EXISTS
to the clients that have a mailbox selected, and STATUS counts grow as
the download goes. An account of 20000 messages takes about 160
requests; at the 0.4 s a request took from the developer's location,
about a minute (an estimate, not measured).

Events keep being applied meanwhile, as proton-bridge applies them
during its sync. A page is requested holding no lock, so a slow or hung
request keeps nothing waiting, and stored under the account's lock
(`user.syncSem`), which events and actions take too. A message that
arrives during the download gets the next UID when its event is
applied, possibly below older ones listed later, and the page that lists
it again leaves it alone: no UID is ever given twice.

A page fetched before an event and stored after it can undo the event
(bring back a deleted message, old flags). So, as proton-bridge does,
peroxide notes the last event applied when the download begins, and
once every message is stored, applies the events after it again
(rewinding the events, bridge's `RewindEventID`); applying an event
twice changes nothing.

For a large account, run the download before the first client connects
(see
[../commands.md](../commands.md#download-mailbox)):

    peroxide download-mailbox <username>

## Rebuilding the database

The database is rebuilt from scratch, never repaired in place: Proton is
the source of truth. A rebuild wipes the database and downloads the
account again; every mailbox gets a new UIDVALIDITY, so clients drop
what they cached and fetch again. Nothing in Proton changes, and you stay
logged in (`auth.json` is a separate file). The `\Deleted` flags set in
this session are lost.

A rebuild happens:

- when Proton sends a **Refresh** event, or when an event **can't be
  applied** for a reason a retry won't cure (see below);
- with `peroxide download-mailbox -discard-local-copy <username>`;
- when the database is from an older peroxide.

The first two mark the database for a rebuild: its clients get
`* BYE` and are disconnected, and the next login (theirs, as they
reconnect, or after a restart) wipes it and starts the download.

## Proton's events

Proton has no push channel for third-party clients. Every client,
Proton's own web app included, polls `/core/v4/events/<ID>`: "what
changed after event ID?". The answer is a page of changes (messages
created, updated, deleted; labels; addresses; a `Refresh` flag), the ID
to ask from next time, and `More: 1` if another page follows.

peroxide (package `events`):

- polls each logged-in account **every 20 s plus a random 0 to 20 s**
  (a fresh random wait each time, from a ChaCha8 source seeded by the
  OS), as proton-bridge does, so that accounts and peroxide instances
  don't poll Proton in step;
- follows `More` at once, without waiting;
- polls at once when a command asks (NOOP with a mailbox selected, and
  APPEND, wait for the answer; STORE, COPY, MOVE and EXPUNGE don't, see
  [changes.md](changes.md));
- stops polling when the account's last client logs out.

Each page is applied whole, in order, under the account's lock
(`user.syncSem`), and clients are told in the order changes were made:
each change's updates are delivered, one after the other, before the
next change's (`user.sendSem`). Then:

- **Messages**: created ones are added to every mailbox their labels
  name (one the download already stored keeps its UIDs); updates change
  flags and labels (a message given a folder's label appears there; one
  losing a label leaves it); deletes remove it everywhere. Clients with
  an affected mailbox selected get `EXISTS`, `EXPUNGE` or `FETCH
  (FLAGS)`. An update for a message the database lacks is left to the
  download while it runs; after, the message is fetched and stored.
- **Labels**: see [mailboxes.md](mailboxes.md#when-protons-folders-and-labels-change).
- **Addresses**: the address list is updated (see
  [../accounts.md](../accounts.md#keys-that-change-while-peroxide-runs)).
- **Refresh**: Proton says "start over": a rebuild.

After each page, peroxide stores its event ID. The next session resumes
from there, so what changed while peroxide was stopped is applied at
the next login, rather than lost.

A page with a message event that fails on the network, a rate limit
(429) or a server error (5xx) is not stored as applied: it is asked for
again at the next poll, as proton-bridge retries such events. One that
fails otherwise (the database can't store it, Proton refuses the
message) marks the database for a rebuild, so that it never silently
differs from Proton.

If Proton refuses the stored ID (HTTP 400, 404 or 422 with an API code
for an invalid value or ID), peroxide starts again from the latest
event and rebuilds, as for a Refresh. Network errors, 401,
429 and 5xx are retried with the same ID at the next poll.

On the very first login of an account (no ID stored), peroxide asks for
the latest event ID **before** listing folders and starting the
download, so a change made meanwhile comes as an event instead of being
missed (proton-bridge's `StartSyncEventID`).

### What this means in practice

- A message that arrives, or a change made in Proton's web or mobile
  apps, reaches your client within 20 to 40 seconds while it is
  connected, or at once on a NOOP sent with a mailbox selected. aerc's
  check-mail sends STATUS for the folders it knows, which reads the
  counts peroxide already has and polls nothing.
- With no client connected, peroxide makes no requests at all: the
  download, like polling, runs only while a client is logged in.
- IDLE works, but adds nothing over polling: updates arrive at the
  poll, not at the moment Proton gets the mail.

## UIDs

RFC 3501 requires that a UID, under one UIDVALIDITY, always names the
same message. peroxide keeps each mailbox's UIDs and UIDVALIDITY in the
database, across sessions and restarts; only a rebuild changes them,
and then UIDVALIDITY changes too, so clients that cache by UID
(Thunderbird, mbsync, aerc's header cache) know to drop their cache.

UIDVALIDITY is the time the mailbox was made, in seconds since
2023-02-01, as gluon (proton-bridge's IMAP library) gives it, and always
above any given before by that database. If the system clock reads a
date before 2023-02-01 (a dead CMOS battery resets it to the BIOS date),
peroxide refuses to make a mailbox, saying so, rather than give a
UIDVALIDITY that may repeat: set the clock right.

Deleting `<username>.db` with peroxide stopped is a rebuild too, but
then the last UIDVALIDITY given is forgotten: if the clock went back
meanwhile, a value may repeat. Prefer `download-mailbox
-discard-local-copy`.

Going back to an older peroxide: its database layout is older, and it
doesn't know the newer one. Delete `<username>.db` first, with peroxide
stopped; the older version then downloads what it needs, as it always
did.

`\Deleted` is kept in memory only (Proton has no such flag), per
mailbox: it is lost when peroxide restarts, which RFC 3501 allows.

## How proton-bridge does it

proton-bridge syncs the whole account in the background after login,
newest first (`GetMessageMetadataPage` with `Desc` and `EndID`, 128 per
page): every message's metadata, then every body, into Gluon's
encrypted store. Events received meanwhile are applied as they come,
and replayed from `StartSyncEventID` once the sync ends. Its UIDs
persist, and UIDVALIDITY comes from the same epoch. On a Refresh it
deletes the account's data and syncs again; an event that fails on the
network, a 429 or a 5xx is retried, and on one it can't apply otherwise
it reports a "bad event" and offers a resync; its `repair` command does
the same, and a resync asked for is remembered (`ShouldResync`) until
the next login.

peroxide downloads the same metadata the same way, but no bodies: each
FETCH of a body still makes a request (`TODO.md` section 9). An event
it can't apply rebuilds at once rather than waiting for you to choose,
as there is no window to ask in.
