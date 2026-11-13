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
messages by their Proton IDs, and the others are applied.

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
- **Into `Trash`** (COPY or MOVE), Proton also unstars them and removes
  their labels (seen 2026-11-10: one `PUT /messages/label` to Trash, and
  the events remove Starred and every `Labels/` label too). peroxide
  takes `\Flagged` off at once, with the answer; the labels leave at the
  next events. Proton's web client does the same.

No `COPYUID` is returned (no UIDPLUS).

## MOVE

RFC 6851 asks that each message end in the source or the destination,
never in neither, and not in both, even if the command fails.

A MOVE is two calls: label with the destination,
then unlabel from the source. Proton keeps a message in one folder, so
labelling it into a folder already takes it out of the previous one,
and the unlabel then answers "does not exist", which counts as done.

**Labels**:

- a MOVE **into** `Labels/<name>` from a folder or system mailbox only
  adds the label: one call, and the message stays where it was. aerc's
  `:modify-labels +name` is this MOVE.
- a MOVE **out of** a label, into a folder or another label, adds the
  destination and removes the label: two calls, as above. aerc's
  `:modify-labels -name` moves to INBOX, so it also files the message
  in INBOX.

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
  but by being deleted), and between INBOX and Sent;
- into or out of **Starred**, which, like All Mail, shows messages that
  stay in their folders. A move into it would take the message out of
  its folder, a move out of it would file it elsewhere to unstar it.
  Star and unstar with the `\Flagged` flag (or COPY to Starred).

A MOVE into the mailbox itself does nothing.

## EXPUNGE

EXPUNGE removes the messages marked `\Deleted` **from this mailbox**.
Messages are deleted for good **only from Trash, Spam and Drafts**, the
mailboxes Proton's web client deletes for good from:

| Expunged from | What happens |
|---|---|
| Trash, Spam, Drafts | deleted for good, unless another mailbox or label holds them, or they are starred (then only removed from here) |
| any other folder: INBOX, Sent, Archive, `Folders/...` | **moved to Trash**, answered `OK [ALERT] N messages moved to Trash: messages are deleted for good only from Trash, Spam or Drafts` |
| a label (`Labels/...`) or Starred | the label is removed; the message stays in its folder |
| All Mail | refused |

- **From a folder**, the move is the same as `MOVE` to Trash: label
  Trash, then unlabel the folder (two requests). A message the move
  fails for stays in the folder, still marked `\Deleted`, for the next
  EXPUNGE. As any move to Trash, it unstars them and removes their
  labels (COPY above). So aerc's `:delete`
  (mark `\Deleted`, expunge in place) does what the web client's
  Delete does, and a second `:delete` in Trash removes the message for
  good.
- **From Trash, Spam or Drafts**, peroxide first looks up what else
  holds each message (one metadata request per 150), and **deletes for
  good** (`PUT /messages/delete`) those no other mailbox or label
  shows. Only the others are then unlabelled from here. A message whose
  delete fails is left exactly as it was, still marked `\Deleted`:
  EXPUNGE answers NO for it, and the next EXPUNGE tries again. If the
  lookup fails, nothing is expunged and the answer is NO.
- **In All Mail, EXPUNGE is refused**: nothing can leave All Mail
  except by being deleted, and deleting everything marked there is
  rarely what a client means.

No message is ever left in All Mail alone by an EXPUNGE, where a
client that doesn't show All Mail couldn't reach it.

`CLOSE` expunges too (RFC 3501), with the same effect. go-imap runs it
even in a mailbox opened with EXAMINE, which RFC 3501 forbids: don't
rely on EXAMINE to protect messages marked `\Deleted`.

## APPEND

APPEND works into every mailbox but **All Mail** and **Starred**. So a
client can copy or move mail from another
account into Proton, and save its copy of what it sent.

**Into `Drafts`** the message is imported like anywhere else, but
neither received nor sent, which is what makes it a draft in Proton, and
read. Its parts and header are kept, as other servers keep them. Its
address is the From address (Sender's if there's no From) if it is one
of yours (a `+alias` ignored) that is enabled and can send, your
primary address otherwise.
It is not made a Proton draft: a draft keeps one body (HTML if there is
one) and none of the original header, so a `multipart/alternative`
message copied from another account would come back HTML only, from
you. Proton's web client lists and opens imported
drafts; a draft aerc postpones is one of them, and recalling and
postponing it again saves a new one (aerc gives it a new
`Message-Id`) and deletes the old.

**Anywhere else** it imports the message (`POST
/mail/v4/messages/import`), with:

- **labels**: the mailbox, and Starred if the message has `\Flagged`;
- **received or sent**: received into INBOX, sent into Sent; elsewhere
  received if the message has a `Received` header field, sent if not;
  replied if it has `\Answered`;
- **unread** unless it has `\Seen`;
- **address**: the From address (or Sender's, if there's no From) if it
  is one of yours, a `+alias` ignored, and can send; your primary
  address otherwise;
- **encryption**: to the first unlocked active key of that address.
  Each part is encrypted on its own, keeping the
  MIME structure (text decoded to UTF-8), so that Proton can read the
  text and list the attachments. A message already signed or
  encrypted, or one with more than one text part, is encrypted whole as
  PGP/MIME instead. A message without a text part gets an empty one
  first: Proton reads a message's text from it. A
  PGP/MIME message (`multipart/encrypted` or `multipart/signed`) is left
  as it is, so that clients still find it at the root and decrypt or
  verify it.

**A message already in your account isn't imported.** Proton keeps one
copy of a message: importing one it has (by its `Message-Id`) makes no
new message but files the one it has in the mailbox, and a mailbox that
is a folder (INBOX, Sent, Spam, Trash, Archive, your folders) takes it
out of the folder it was in. Moving one message from another account
into Spam, then Archive, then Trash moved Proton's single copy from
folder to folder, each APPEND answering OK, and the client deleted its
source each time. So before importing, peroxide looks the message up by
its `Message-Id` among the account's messages (one metadata
request).

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

**Size**: 70 MiB (73400320 bytes), the import API's limit, on the
encrypted message: about 50 MB of
attachments once base64-encoded, Proton's limit for received mail.
Encrypted part by part, a message keeps about its size; encrypted
whole it grows by a third, and over 70 MiB is refused with `NO
[TOOBIG]`. A larger literal is refused with `BAD` before it is read
([README.md](README.md#limits-worth-knowing)).

APPEND waits for the next events before it answers, so the new message
is visible when it does. The date an APPEND gives (its INTERNALDATE) isn't
kept: Proton dates an imported message by its `Date`
field.

## CREATE, DELETE, RENAME

Only folders and labels can be made, renamed and deleted, under the
names peroxide shows them by ([mailboxes.md](mailboxes.md)):
`Folders/<path>` for a folder, at any depth, and `Labels/<name>` for a
label, which can't be nested. Every other name is refused with NO,
before any request: the system mailboxes, INBOX included, the
`Folders` and `Labels` parents, and names outside them
(`Archive/2026`). A level written with U+2215 (as peroxide shows a `/`
in a name) becomes `/` in Proton.

Each makes one request to Proton (plus one per missing parent folder
CREATE or RENAME makes first), and peroxide changes the
mailbox list at once from Proton's answer, so the client can open what
it just created; the `Labels` event that follows changes nothing.

| Command | Proton call | Notes |
|---|---|---|
| CREATE | `POST /core/v4/labels` | color `#f66`; missing parent folders are created first, one call each |
| DELETE | `DELETE /core/v4/labels/{id}` | clients that have it selected get BYE, as for a delete in the web client |
| RENAME | `PUT /core/v4/labels/{id}` | keeps the color; a folder can move into another (created if missing) |

- **CREATE of a mailbox there is** answers `NO Mailbox already exists`
  and makes no request. aerc sends it before every APPEND, and ignores
  the answer.
- **RENAME** keeps the mailbox's messages, UIDs and UIDVALIDITY; the
  folders inside a renamed folder follow it. A subfolder can move to the
  top of `Folders` (`RENAME Folders/x/c Folders/c`): peroxide sends
  `ParentID` 0, as Proton's web client does when you drag a folder to
  the top. Refused: a folder to a label or back, and a folder into
  itself.
- **DELETE of a folder** deletes the folders inside it too (Proton
  does; peroxide removes them at once, with a BYE each), and leaves its
  messages in no folder: they show in All Mail only (seen 2026-11-09).
  Move them out first if you want them elsewhere. A label's messages
  just lose the label.
- **Proton's own limits** reach the client in the NO: on a free plan,
  `[2011] Folder limit reached`.
- **SUBSCRIBE, UNSUBSCRIBE** are kept by peroxide alone
  ([mailboxes.md](mailboxes.md#list-lsub-subscribe)).

aerc: `:mkdir Folders/Work` creates and opens a folder, `:mkdir
Labels/Todo` a label, `:rmdir` deletes the open one (`-f` if it isn't
empty). `:move -p` and `:copy -p` create a missing destination. aerc
has no command to rename, and shows the parents `:mkdir Folders/a/b`
created, and drops what `:rmdir` deleted, only after it reconnects.

<!-- nav -->

---

[Previous: Messages: FETCH, decryption and what can't be decrypted](messages.md) | [Contents](../README.md) | [Next: Sending mail: the SMTP server and `peroxide sendmail`](../smtp.md)
