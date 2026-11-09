# Changing mail: STORE, COPY, MOVE, EXPUNGE, APPEND

Every change is one or two Proton API calls. peroxide answers the
client as soon as Proton has answered, applies the result to the local
database itself, tells the clients that have the mailbox selected, and
asks for Proton's events right after without waiting for them. The
events then confirm the change and bring its side effects (moving a
message into a folder takes it out of the previous one, for instance).
From a distant location a Proton round trip is about 0.4 s, so most
commands take that long.

Proton answers an action on several messages with one result per
message. A message it reports as "does not exist" (code 2501) for an
action is treated as already done (marking a read message read); any
other per-message failure fails the command with NO naming the
messages, and the others are applied.

## STORE

| Flag | Proton call |
|---|---|
| `\Seen` | `PUT /messages/read` or `/messages/unread` |
| `\Flagged` | label or unlabel Starred |
| `\Deleted` | none: kept by peroxide for the mailbox, until EXPUNGE |
| `\Draft`, `\Answered`, keywords, others | ignored |

`+FLAGS` and `-FLAGS` change what they name. `FLAGS` replaces: it adds
every flag listed, then removes, from the messages that have them, the
flags not listed. Each flag is one call, so `FLAGS (\Seen Work)` on
messages that had `Home` is three calls.

Clients with the mailbox selected get `FETCH (FLAGS ...)`, the one that
sent the STORE too unless it used `.SILENT`.

`\Deleted` lives in peroxide's memory, per mailbox, shared by every
connection that has it selected: it is lost when peroxide restarts.

## COPY

One call: label the messages with the destination
(`PUT /messages/label`). The messages stay where they were.

- **Into a folder**, Proton moves them: a message has one folder, so
  copying into one takes it out of the previous folder. The source
  shows the EXPUNGE at the next events. This is Proton's model, not
  peroxide's choice: there is no way to have one message in two folders.
- Into `Starred`, it stars them.
- **Into `Labels/<name>`**, it adds the label; the messages stay in
  their folder.
- **Into `Trash`** (COPY or MOVE), Proton also unstars them. peroxide
  takes `\Flagged` off at once, with the answer, rather than at the next
  events.

No `COPYUID` is returned (no UIDPLUS).

## MOVE

RFC 6851 asks that each message end in the source or the destination,
never in neither, and not in both, even if the command fails.

A MOVE is two calls, as in proton-bridge: label with the destination,
then unlabel from the source. Proton keeps a message in one folder, so
labelling it into a folder already takes it out of the previous one,
and the unlabel then answers "does not exist", which counts as done.

**Labels**, as in proton-bridge (`shouldExpungeOldLocation`):

- a MOVE **into** `Labels/<name>` from a folder or system mailbox only
  adds the label: one call, and the message stays where it was. aerc's
  `:modify-labels +name` is this MOVE.
- a MOVE **out of** a label, into a folder or another label, adds the
  destination and removes the label: two calls, as above. aerc's
  `:modify-labels -name` moves to INBOX, so it also files the message
  in INBOX, as with bridge.

peroxide:

- unlabels only the messages that were labelled (one the label call
  failed for stays where it was);
- for a message the unlabel failed for, reads its labels back from
  Proton. If Proton already took it out of the source, the move is
  complete. Otherwise it unlabels the message from the destination
  again, as if it never moved. If that undo fails too, the message is
  in both mailboxes, in Proton as well as locally, and the NO says so:
  remove it from one by hand. If only the check fails, the local view
  shows both until the next events tell what Proton did.

**Refused**, before any call:

- into or out of **All Mail** (every message is there; none leaves it
  but by being deleted), and between INBOX and Sent, as proton-bridge
  refuses them;
- into or out of **Starred**, which, like All Mail, shows messages that
  stay in their folders. A move into it would take the message out of
  its folder, a move out of it would file it elsewhere to unstar it.
  Star and unstar with the `\Flagged` flag (or COPY to Starred).

A MOVE into the mailbox itself does nothing.

## EXPUNGE

EXPUNGE removes the messages marked `\Deleted` **from this mailbox**.
It does not delete them from your account, except from Trash or Drafts:

- From a folder, the messages are unlabelled from it
  (`PUT /messages/unlabel`), and left in All Mail (and any labels they
  have). Proton puts no folder in their place.
- **From Trash or Drafts**, peroxide first looks up what else holds
  each message (one metadata request per 150), and **deletes for good**
  (`PUT /messages/delete`) those no other mailbox or label shows. Only
  the others, still in another folder or with a label, are then
  unlabelled from Trash. A message whose delete fails is left exactly
  as it was: in Trash, still marked `\Deleted`. EXPUNGE then answers NO
  for it, and the next EXPUNGE tries again. If the lookup fails, nothing
  is expunged and the answer is NO.
- **In All Mail, EXPUNGE is refused**: nothing can leave All Mail
  except by being deleted, and deleting everything marked there is
  rarely what a client means. Move messages to Trash and expunge there.

Deleting before unlabelling means a message expunged from Trash is
either gone or still in Trash: never left in All Mail alone, where a
client that doesn't show All Mail couldn't reach it, and never
announced as expunged when it wasn't.

So the usual "delete" of a client (move to Trash, then expunge Trash
when emptying it) behaves as in Proton's web app. A client that
"deletes" by marking `\Deleted` and expunging in place (mutt with
`$trash` unset, some IMAP scripts) archives to All Mail instead.

`CLOSE` expunges too (RFC 3501), with the same effect. go-imap runs it
even in a mailbox opened with EXAMINE, which RFC 3501 forbids: don't
rely on EXAMINE to protect messages marked `\Deleted`.

## APPEND

APPEND works into every mailbox but **All Mail** and **Starred**, as
in proton-bridge. So a client can copy or move mail from another
account into Proton, and save its copy of what it sent.

**Into `Drafts`** the message is imported like anywhere else, but
neither received nor sent, which is what makes it a draft in Proton, and
read. Its parts and header are kept, as other servers keep them. Its
address is the From address (Sender's if there's no From) if it is one
of yours that is enabled and can send, your primary address otherwise.
proton-bridge makes a Proton draft instead (`createDraftWithParser`),
which keeps one body (HTML if there is one) and none of the original
header: a `multipart/alternative` message comes back HTML only, from
you. peroxide did the same until it was seen to mangle messages copied
from other accounts. Proton's web client lists and opens imported
drafts; a draft aerc postpones is one of them, and recalling and
postponing it again saves a new one (aerc gives it a new
`Message-Id`) and deletes the old.

**Anywhere else** it imports the message (`POST
/mail/v4/messages/import`), with what bridge sends:

- **labels**: the mailbox, and Starred if the message has `\Flagged`;
- **received or sent**: received into INBOX, sent into Sent; elsewhere
  received if the message has a `Received` header field, sent if not;
  replied if it has `\Answered`;
- **unread** unless it has `\Seen`;
- **address**: the From address (or Sender's, if there's no From) if it
  is one of yours, a `+alias` ignored, and can send; your primary
  address otherwise;
- **encryption**: to the first unlocked active key of that address. As
  go-proton-api does, each part is encrypted on its own, keeping the
  MIME structure (text decoded to UTF-8), so that Proton can read the
  text and list the attachments. A message already signed or
  encrypted, or one with more than one text part, is encrypted whole as
  PGP/MIME instead. A message without a text part gets an empty one
  first, as bridge adds it: Proton reads a message's text from it. A
  PGP/MIME message (`multipart/encrypted` or `multipart/signed`) is left
  as it is, so that clients still find it at the root and decrypt or
  verify it; bridge would wrap it.

**A message already in your account isn't imported.** Proton keeps one
copy of a message: importing one it has (by its `Message-Id`) makes no
new message but files the one it has in the mailbox, and a mailbox that
is a folder (INBOX, Sent, Spam, Trash, Archive, your folders) takes it
out of the folder it was in. Moving one message from another account
into Spam, then Archive, then Trash moved Proton's single copy from
folder to folder, each APPEND answering OK, and the client deleted its
source each time. So before importing, peroxide looks the message up by
its `Message-Id` among the account's messages (one metadata request, as
bridge looks a Message-Id up).

That finds a message sent through peroxide's SMTP too: SMTP gives Proton
each message's `Message-Id`, and Proton finds it by it at once (tested
2026-11-09: aerc's `copy-to = Sent` copies, plain and with an
attachment, each looked up in the second of the send, all found). A
message without a `Message-Id` (clients set one; a hand-made `sendmail`
input may not) is imported: its saved copy is then in Sent twice.

Not into **Drafts**: a client saving a draft again may keep its
`Message-Id`, and deletes the old draft once the APPEND answers OK, so
"already there" would lose the edit. A draft is always imported, and
Proton doesn't merge drafts by `Message-Id`: tested 2026-11-09, two
drafts APPENDed under one `Message-Id` became two messages (two
MessageIDs, two create events), and deleting the first, as a client
saving again does, left the second, edited one.

Found, nothing changes in Proton, and APPEND answers with an `ALERT`,
the one response code clients must show (RFC 3501 section 7.1):

- in this mailbox already (a copy of a sent message saved into Sent,
  aerc's `copy-to`): `OK [ALERT] This message is already in Sent: it
  was not copied again`;
- elsewhere: `NO [ALERT] This message is already in your Proton
  account, in INBOX: it was not copied, as Proton keeps one copy of a
  message`. A client moving it from another account keeps its copy
  there, as a move whose APPEND fails deletes nothing.
- in the other of INBOX and Sent: Proton keeps a message in both (mail
  to yourself), so it is added to this one, as COPY does, moving
  nothing: `OK [ALERT] This message is already in Sent: it is now in
  INBOX too, not copied again`. This is what aerc's `copy-to-replied`
  asks after a reply.
- anywhere, APPENDed into `Labels/<name>`: the label is added, moving
  nothing: `OK [ALERT] This message is already in Archive: it is now in
  Labels/name too, not copied again`.

A message the account doesn't have is refused from `Labels/<name>`:
`NO [ALERT] Labels/name is a label: ... Append it to a folder, then add
the label`. Imported with the label alone it would be in All Mail only.

Either way the message found is left as it is: the flags the APPEND
gives (`\Flagged`, `\Seen`) aren't applied to it.

Thunderbird and Evolution show `ALERT` texts; aerc and KMail show
neither OK nor `ALERT` texts, only NO's.

**Size**: 70 MiB (73400320 bytes), the import API's limit as
go-proton-api checks it, on the encrypted message: about 50 MB of
attachments once base64-encoded, Proton's limit for received mail.
Encrypted part by part, a message keeps about its size; encrypted
whole it grows by a third, and over 70 MiB is refused with `NO
[TOOBIG]`. A larger literal is refused with `BAD` before it is read
([README.md](README.md#limits-worth-knowing)).

APPEND waits for the next events before it answers, so the new message
is visible when it does. The date an APPEND gives (its INTERNALDATE) isn't
kept, as in bridge: Proton dates an imported message by its `Date`
field.

## How proton-bridge does it

- **STORE, COPY, MOVE** reach Proton through Gluon, Proton's IMAP
  library, which keeps the mailbox state in its own store. Labels are
  mailboxes there, so COPY to `Labels/x` adds a label and MOVE out of
  one removes it. Its MOVE has two forms behind a feature flag
  (connector.go, unleash `FolderUnlabelCallDisabled`, off unless
  Proton's server turns it on): by default
  `moveMessagesWithUnlabelCallOnFolders`, label then unlabel, which
  peroxide follows; the other, `moveMessagesWithoutUnlabelCallOnFolders`,
  skips the unlabel between folders. Bridge dropped the call in 3.24.0
  (BRIDGE-451) and restored it under the flag in 3.24.1 (BRIDGE-522), as
  its comment says "removing the unlabel call resulted in a lot of
  client issues". Both forms refuse MOVE between INBOX and Sent and into
  or out of All Mail and Scheduled. Bridge allows MOVE into and out of
  Starred; peroxide refuses it (above). Labels follow bridge: a MOVE
  into a label from a folder skips the unlabel, and every MOVE out of
  one unlabels (bridge then also expunges the message from the source
  mailbox in Gluon).
- **EXPUNGE**: bridge unlabels first, then deletes for good, from Trash
  and Drafts, what no other shown label holds; All Mail refuses.
  peroxide decides the same, in the other order, so that a failed
  delete leaves the message in Trash rather than only in All Mail,
  which bridge can hide ("Show All Mail" off).
- **APPEND** (`Connector.CreateMessage`) imports the message into any
  mailbox but All Mail and Scheduled, through go-proton-api's
  `ImportMessages`, with the labels, flags, address and encryption
  above; Drafts makes a draft. peroxide decides the same, and also
  refuses Starred, as it refuses moves into it: bridge imports there
  with the Starred label alone, leaving the message in no folder.
  peroxide shows no Scheduled mailbox. Into a label, bridge imports a
  new message with that label alone, in no folder; peroxide refuses it
  and labels only a message the account already has.
- **A message already there**: bridge's `sendrecorder` remembers sends
  for 30 minutes, by gluon's `GetMessageHash` and the recipients, and
  APPEND waits up to 90 seconds for one under way; gluon then adds the
  existing message to the mailbox, and answers OK. Any other message
  bridge imports, and Proton's own deduplication files the message it
  has in the mailbox, moving it from its folder, under a plain OK.
  peroxide keeps no record of sends: it looks up the `Message-Id`,
  which finds sent messages as well (above), and then imports nothing
  and moves nothing: OK [ALERT] if the message is in the mailbox
  already (or in INBOX for Sent, and back), NO [ALERT] otherwise.
- **Size**: bridge checks only go-proton-api's 70 MiB on the encrypted
  message, and announces no APPENDLIMIT. peroxide announces the same
  70 MiB.
