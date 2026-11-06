# Mailboxes, folders, labels and flags

## What Proton has

Proton files every message under **labels**, each with an ID. Some are
built in (system labels, fixed small-number IDs); you create the
others, as two kinds:

- **Folders** (`Exclusive: 1`): a message is in at most one folder,
  like a classic mailbox. Inbox, Archive, Spam, Trash, Sent and Drafts
  behave as folders too.
- **Labels** (`Exclusive: 0`): tags, any number per message.

A message is also always in **All Mail** (label 5), and in the
"all drafts" (1) and "all sent" (2) labels as fits. Starred (10) is a
label you set by starring.

## What peroxide shows

| IMAP mailbox | Proton label ID | LIST attributes |
|---|---|---|
| `INBOX` | 0 | |
| `All Mail` | 5 | `\All` |
| `Archive` | 6 | `\Archive` |
| `Drafts` | 8 | `\Drafts` |
| `Starred` | 10 | `\Flagged` |
| `Spam` | 4 | `\Junk` |
| `Sent` | 7 | `\Sent` |
| `Trash` | 3 | `\Trash` |
| each of your folders | its ID | |

Every mailbox also has `\Noinferiors`: peroxide shows no hierarchy.
A folder's name is Proton's name for it, as is. Nested Proton folders
appear under their own name, not their path (`TODO.md` section 1).

**Your labels are not mailboxes: they are IMAP flags (keywords).** A
label named `To do` is the flag `To_do` on every message that has it.
The name is made a valid IMAP atom: characters RFC 3501 forbids in an
atom (space, `(`, `)`, `{`, `%`, `*`, `"`, `\`, `]`, controls and
non-ASCII) become `_`, runs of them one `_`. Setting the flag on a
message adds the label in Proton; removing it removes the label.

Two system labels are flags as well: Starred is `\Flagged`, and Drafts
is `\Draft` (read-only: drafts are a mailbox in Proton, so `\Draft`
can't be set or cleared).

Flags peroxide maps:

| IMAP flag | Proton | Settable |
|---|---|---|
| `\Seen` | not `Unread` | yes |
| `\Answered` | `IsReplied` or `IsRepliedAll` | no (ignored) |
| `\Flagged` | label Starred | yes |
| `\Draft` | label Drafts | no |
| `\Deleted` | kept by peroxide only, per mailbox, until EXPUNGE or restart | yes |
| each label's flag | that label | yes |

Flags are matched without regard to case: go-imap lower-cases every
flag RFC 3501 doesn't define, so `To_do`, `to_do` and `TO_DO` reach the
same label. If two labels differ only in case, or map to the same atom
(`To do` and `To_do`), a flag reaches the one with the lowest ID. SELECT
lists `\Seen \Deleted` then the label flags, in label ID order, in both
`FLAGS` and `PERMANENTFLAGS`. Any other flag a client sets is ignored
(Proton has nowhere to keep it), and the client isn't told.

## LIST, LSUB, SUBSCRIBE

LIST returns every mailbox above (the order varies from one LIST to the
next, `TODO.md` section 11). LSUB returns the same, as peroxide treats
every mailbox as subscribed; SUBSCRIBE and UNSUBSCRIBE fail with "not
yet implemented". Clients that LIST only subscribed mailboxes see them
all.

## STATUS and SELECT counts

Until a mailbox has been used in this session (see
[sync.md](sync.md)), its counts are Proton's: listed at login
(`/messages/count`) and fetched again after every event page that
changed a message. Once used, they are counted from the local database,
which events keep current.

`UIDVALIDITY` is always 1, `RECENT` always 0, and SELECT gives no
`[UNSEEN n]`. See [sync.md](sync.md#uids) for what that means.

## When Proton's folders and labels change

Proton sends a `Labels` event when you create, rename or delete a
folder or label (on the web, on your phone, anywhere). peroxide applies
it within one poll (20 to 40 seconds while a client is connected):

- A created folder becomes a mailbox; a created label, a flag.
- A renamed folder keeps its messages and its local data (everything is
  keyed by label ID); clients that have it selected keep it selected
  under the new name.
- A deleted folder's mailbox and local entries go (its messages stay in
  the database, as other mailboxes may hold them).
- A folder turned into a label, or the reverse, is ignored until the
  next login.

**Clients are not told.** go-imap could send an unsolicited LIST, but
its own client library, which aerc uses, drops it. Your client sees
the new mailbox list the next time it sends LIST. aerc sends LIST only
when it connects, so reconnect it (`:connect`, or restart it).

## Known collisions

- Two folders with the same name (at different levels in Proton, or one
  named like a system mailbox, "Sent") can't both be opened: a SELECT
  of that name opens whichever peroxide finds first.
- A folder named `Inbox` (any case) can't be opened at all: IMAP
  reserves that name (RFC 3501 section 5.1), and go-imap turns it into
  `INBOX` before peroxide sees it.

`TODO.md` section 1 plans names that remove both, the way proton-bridge
does.

## How proton-bridge does it

proton-bridge shows folders as `Folders/<path>` and labels as
`Labels/<name>`, both as mailboxes, with system mailboxes at the top
level. A message with three labels then appears in three `Labels/`
mailboxes plus its folder; COPY to `Labels/x` adds a label, EXPUNGE
from it removes one. Names can't collide, nesting shows, and aerc
recognises `Labels/` as labels when the server is on 127.0.0.1.

peroxide shows labels as flags, as hydroxide did: a message appears
once per folder, and clients filter by keyword instead. Labels as
mailboxes are planned (`TODO.md` section 1).

proton-bridge also hides All Mail by default (setting "Show All
Mail"), and refuses APPEND and moves into it. peroxide always shows it
(`TODO.md` section 8).
