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
| `INBOX` | 0 | `\Noinferiors` |
| `Drafts` | 8 | `\Noinferiors \Drafts` |
| `Sent` | 7 | `\Noinferiors \Sent` |
| `Starred` | 10 | `\Noinferiors \Flagged` |
| `Archive` | 6 | `\Noinferiors \Archive` |
| `Spam` | 4 | `\Noinferiors \Junk` |
| `Trash` | 3 | `\Noinferiors \Trash` |
| `All Mail` | 5 | `\Noinferiors \All` (hidden from LIST with `-show-all-mail=false`) |
| `Folders` | none | `\Noselect` |
| `Folders/<path>`, each of your folders | its ID | `\HasChildren` or `\HasNoChildren` |
| `Labels` | none | `\Noselect` |
| `Labels/<name>`, each of your labels | its ID | `\Noinferiors` |

These are proton-bridge's names. `Folders` and `Labels` hold no
messages: they are there so clients draw the hierarchy, and every
command but LIST refuses them. A folder inside another is
`Folders/<parent>/<name>`, to any depth; labels can't be nested in
Proton.

Names are built from each folder's own name and its parent, never from
Proton's `Path`: Proton escapes a `/` in a name as `\/` there but leaves
`\` alone, so a folder `A/B` and a folder `B` inside `A\` have the same
`Path`. A `/` in a name would read as nesting, so peroxide shows it as
U+2215 (DIVISION SLASH), which looks the same: the folder `A/B` is
`Folders/A\u2215B`. A `\` is shown as is.

A message with three labels appears in its folder and in three
`Labels/` mailboxes. To label a message, COPY or MOVE it into
`Labels/<name>` (see [changes.md](changes.md)); to remove the label,
EXPUNGE it from there or MOVE it out.

Two system labels are flags: Starred is `\Flagged`, and Drafts is
`\Draft` (read-only: drafts are a mailbox in Proton, so `\Draft` can't
be set or cleared).

Flags peroxide maps:

| IMAP flag | Proton | Settable |
|---|---|---|
| `\Seen` | not `Unread` | yes |
| `\Answered` | `IsReplied` or `IsRepliedAll` | no (ignored) |
| `\Flagged` | label Starred | yes |
| `\Draft` | label Drafts | no |
| `\Deleted` | kept by peroxide only, per mailbox, until EXPUNGE or restart | yes |

SELECT lists `\Seen \Deleted \Flagged \Draft` in both `FLAGS` and
`PERMANENTFLAGS`. Any other flag a client sets, keywords included, is
ignored (Proton has nowhere to keep it), and the client isn't told.

## LIST, LSUB, SUBSCRIBE

LIST returns every mailbox above, always in the order of the table:
system mailboxes, then `Folders` and the folders by name, then `Labels`
and the labels by name.

Subscriptions are peroxide's alone: Proton has none, so they are kept
in the local database, and other devices don't see them. Every mailbox
starts subscribed, a new one too. UNSUBSCRIBE takes a mailbox out of
LSUB, SUBSCRIBE puts it back; doing either twice answers OK. LSUB lists
the subscribed mailboxes in LIST's order, and `Folders` or `Labels`
while any mailbox inside is subscribed. A renamed mailbox keeps its
subscription; a deleted one leaves LSUB. A rebuild of the database
keeps them; an upgrade that changes the database's layout starts every
mailbox subscribed again. aerc only uses LIST, so subscriptions change
nothing there; Evolution and Thunderbird read LSUB.

### How proton-bridge does it

gluon keeps a subscribed flag per mailbox in its database, true for a
new one, lost with the database on a resync. It answers NO to
SUBSCRIBE of a subscribed mailbox and UNSUBSCRIBE of an unsubscribed
one. A subscribed mailbox that is deleted stays in LSUB, `\Noselect`,
until the client unsubscribes, as RFC 3501 section 6.3.9 asks; peroxide
drops it, so that LSUB clients don't show a deleted mailbox for good.

## STATUS and SELECT counts

Counted from the local database, which the download fills and events
keep current: while the first download runs, they grow with it (see
[sync.md](sync.md#the-download)).

`UIDVALIDITY` is the mailbox's own, kept across restarts; `RECENT` is
always 0, and SELECT gives no `[UNSEEN n]`. See [sync.md](sync.md#uids).

## When Proton's folders and labels change

Proton sends a `Labels` event when you create, rename or delete a
folder or label (on the web, on your phone, anywhere). peroxide applies
it within one poll (20 to 40 seconds while a client is connected):

- A created folder or label becomes a mailbox, filled from the messages
  already stored: nothing is downloaded again.
- A renamed or moved folder or label keeps its messages, UIDs and
  UIDVALIDITY (everything is keyed by label ID); clients that have it
  selected keep it selected under the new name. The folders inside a
  renamed folder are renamed with it. Proton sends no event for them,
  so peroxide also reads the labels once more (`GET /labels`) and logs
  any name that differs from the one it derived.
- A folder turned into a label, or the reverse, moves from `Folders/`
  to `Labels/` (or back) the same way, keeping its UIDs.
- A deleted folder or label's mailbox and local entries go (its
  messages stay in the database, as other mailboxes may hold them).
  Clients that have it selected get `* BYE <name> was deleted in
  Proton: please log in again`; aerc reconnects by itself. peroxide
  doesn't close the connection after the BYE (go-imap's server doesn't):
  a client that ignores it keeps a connection whose commands fail
  (`TODO.md` section 14).

Folders and labels a client creates, renames or deletes through
peroxide ([changes.md](changes.md#create-delete-rename)) change at
once; the event that follows changes nothing.

**Other clients are not told.** go-imap could send an unsolicited LIST,
but its own client library, which aerc uses, drops it. Your client sees
the new mailbox list the next time it sends LIST. aerc sends LIST only
when it connects, so reconnect it (`:connect`, or restart it).

Seen with aerc and Evolution: neither removes a deleted or renamed
mailbox from its tree after listing again. aerc keeps every directory
it has seen until it restarts, and after the BYE's reconnect tries to
open the deleted one again ("No such mailbox"). Evolution sometimes
lists again after a STATUS fails and still shows it; after a folder was
moved in the web client it never listed again, Send/Receive included,
until restarted. A renamed mailbox therefore
shows under both names until the client is restarted. Evolution draws
`Folders` and `Labels` as parents; aerc's default list shows full
names (`Folders/Work`).

## Names that can't collide

Proton keeps folder names unique among siblings, and labels unique, so
the prefixes make every name unique: a folder named `Sent` is
`Folders/Sent`, and one named `Inbox` is `Folders/Inbox`, which IMAP
doesn't reserve (only a top-level `INBOX` is, RFC 3501 section 5.1).
The one collision left is a name that already holds U+2215, which
reads like one holding `/`.

## How proton-bridge does it

proton-bridge shows folders as `Folders/<path>` and labels as
`Labels/<name>`, both as mailboxes, with system mailboxes at the top
level, as peroxide does. aerc recognises `Labels/` as labels when the
server is on 127.0.0.1, so `:modify-labels` works.

Differences: bridge splits Proton's `Path` on every `/`, so a folder
named `A/B` shows as `B` inside a folder `A\`; peroxide shows
`Folders/A\u2215B`. When a folder is renamed, bridge fetches every
label list once per subfolder (go-proton-api's `GetLabel` downloads the
whole list); peroxide derives the new names and fetches the list once.
gluon also sends BYE to clients whose selected mailbox is deleted.

proton-bridge also has a "Show All Mail" setting (`ShowAllMail`, on
by default); off, All Mail is left out of LIST, and SELECT still opens
it by name. peroxide's `-show-all-mail=false` does the same, for
clients that don't want each message twice. bridge also refuses APPEND
and moves into All Mail, as peroxide does whatever the setting
(`TODO.md` section 8); peroxide refuses them into Starred too.
