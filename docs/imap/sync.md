# The local database, events and UIDs

## The local database

Each account has a bolt database, `~/.config/peroxide/<username>.db`
(package `imap/database`). It holds:

| Bucket | Content |
|---|---|
| `messages` | Each message's metadata as Proton lists it, JSON, keyed by Proton's message ID: subject, addresses, date, size, flags, label IDs, attachment list. No body |
| `mailboxes/<label ID>` | One bucket per mailbox: its messages in order, each with its sequence number and UID |
| `counts` | Total and unread per mailbox, kept as messages change, so STATUS doesn't count |
| `events` | `last`: the ID of the last Proton event applied |

It is not encrypted. See [../accounts.md](../accounts.md#files-on-disk).

## When a mailbox is filled

A mailbox is **synchronized** (`mailbox.sync`) the first time a client
uses it in this session: FETCH, SEARCH, STORE, COPY, MOVE, EXPUNGE or
APPEND. SELECT and STATUS don't: they use Proton's counts until then.
Synchronizing:

1. empties the mailbox's bucket (its UIDs start again from 1);
2. lists the mailbox from Proton, 150 messages per request, oldest
   first (`GET /messages?Desc=0&Label=<ID>&PageSize=150&Sort=ID`, then
   `&Page=1`, ...);
3. stores each message's metadata and gives it the next UID.

A folder of 2000 messages takes 14 requests, about 0.4 s each from a
distant location; aerc waits for them before its first list. Then the
mailbox is current, and stays so through events, until peroxide
restarts. `TODO.md` sections 9 and 9.1.

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

Each page is applied whole, in order, while no mailbox synchronizes
(`user.syncSem`): a sync and an event never interleave. Then:

- **Messages**: created ones are added to every mailbox their labels
  name; updates change flags and labels (a message given a folder's
  label appears there; one losing a label leaves it); deletes remove it
  everywhere. Clients with an affected mailbox selected get `EXISTS`,
  `EXPUNGE` or `FETCH (FLAGS)`.
- **Labels**: see [mailboxes.md](mailboxes.md#when-protons-folders-and-labels-change).
- **Addresses**: the address list is updated (see
  [../accounts.md](../accounts.md#keys-that-change-while-peroxide-runs)).
- **Refresh**: Proton says "start over". Every mailbox is emptied, and
  folders, labels and counts are listed again; mailboxes fill again when
  next used.

After each page, peroxide stores its event ID. The next session resumes
from there, so what changed while peroxide was stopped is applied at
the next login, rather than lost.

If Proton refuses the stored ID (HTTP 400, 404 or 422 with an API code
for an invalid value or ID), peroxide starts again from the latest
event and rebuilds every mailbox, as for a Refresh. Network errors, 401,
429 and 5xx are retried with the same ID at the next poll.

On the very first login of an account (no ID stored), peroxide asks for
the latest event ID **before** listing folders and counts, so a change
made in between comes as an event instead of being missed.

### What this means in practice

- A message that arrives, or a change made in Proton's web or mobile
  apps, reaches your client within 20 to 40 seconds while it is
  connected, or at once on a NOOP sent with a mailbox selected. aerc's
  check-mail sends STATUS for the folders it knows, which reads the
  counts peroxide already has and polls nothing.
- With no client connected, peroxide makes no requests at all.
- IDLE works, but adds nothing over polling: updates arrive at the
  poll, not at the moment Proton gets the mail.

## UIDs

RFC 3501 requires that a UID, under one UIDVALIDITY, always names the
same message. **peroxide does not keep that promise across restarts**:
UIDVALIDITY is always 1, and each session's first sync of a mailbox
gives its messages new UIDs from 1. Within one run of peroxide, UIDs are
stable.

A client that caches by UID (Thunderbird, mbsync, offlineimap, aerc's
header cache) can show the wrong message for a cached UID after
peroxide restarts, or flag the wrong one. Clear the client's cache after
restarting peroxide, or use a client that doesn't cache. `TODO.md`
section 9.1 plans keeping UIDs across sessions.

`\Deleted` is kept in memory only (Proton has no such flag), per
mailbox: it is lost when peroxide restarts, which RFC 3501 allows.

## How proton-bridge does it

proton-bridge syncs the whole account in the background after login:
every message's metadata, then every body, into Gluon's encrypted
store. Its UIDs persist and its UIDVALIDITY changes only when it must.
It polls events at the same 20 + 0..20 s, and on an event it can't
apply it reports a "bad event" and offers a full resync.

peroxide syncs nothing until a client asks, and keeps no bodies, so
its first use of a mailbox costs a full listing each session, and the
UID guarantee is lost on restart. In return its disk and memory use
stay small, and an account no client opens costs nothing.
