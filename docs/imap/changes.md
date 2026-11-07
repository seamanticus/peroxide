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
| a label's flag | `PUT /messages/label` or `/messages/unlabel` with that label |
| `\Deleted` | none: kept by peroxide for the mailbox, until EXPUNGE |
| `\Draft`, `\Answered`, others | ignored |

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

No `COPYUID` is returned (no UIDPLUS).

## MOVE

Two calls: label with the destination, then unlabel from the source.
RFC 6851 asks that each message end in the source or the destination,
never in neither, and not in both, even if the command fails. peroxide:

- unlabels only the messages that were labelled (one the label call
  failed for stays where it was);
- for a message the unlabel failed for, reads its labels back from
  Proton. If Proton already took it out of the source (moving into a
  folder does that), the move is complete. Otherwise it unlabels the
  message from the destination again, as if it never moved. If that
  undo fails too, the message is in both mailboxes, in Proton as well
  as locally, and the NO says so: remove it from one by hand. If only
  the check fails, the local view shows both until the next events
  tell what Proton did.

## EXPUNGE

EXPUNGE removes the messages marked `\Deleted` **from this mailbox**,
by unlabelling them (`PUT /messages/unlabel`). It does not delete them
from your account:

- From a folder, a message unlabelled from its folder is left in All
  Mail (and any labels it has). Proton puts no folder in its place.
- **From Trash or Drafts**, peroxide then deletes for good
  (`PUT /messages/delete`) the expunged messages that no other mailbox
  or label still shows. A message still in another folder, or with a
  label, isn't deleted. If that delete fails, EXPUNGE still answers OK
  (the messages did leave the mailbox, and clients were told); they
  stay in All Mail, and the failure is logged.
- **In All Mail, EXPUNGE is refused**: nothing can leave All Mail
  except by being deleted, and deleting everything marked there is
  rarely what a client means. Move messages to Trash and expunge there.

So the usual "delete" of a client (move to Trash, then expunge Trash
when emptying it) behaves as in Proton's web app. A client that
"deletes" by marking `\Deleted` and expunging in place (mutt with
`$trash` unset, some IMAP scripts) archives to All Mail instead.

`CLOSE` expunges too (RFC 3501), with the same effect. go-imap runs it
even in a mailbox opened with EXAMINE, which RFC 3501 forbids: don't
rely on EXAMINE to protect messages marked `\Deleted`.

## APPEND

APPEND works only into `Drafts`: it creates a Proton draft from the
message (its sender from the From address, which must be one of yours).
Anywhere else it fails with "cannot create messages outside the Drafts
mailbox". So a client can't copy mail from another account into
Proton, or save its own copy of a sent message into `Sent`: the latter
is unnecessary, as Proton files sent mail in Sent itself (see
[../smtp.md](../smtp.md)). Configure your client not to save sent mail
(aerc: no `copy-to`), or you will get an error after each send.

The limit is 50 MiB. APPEND waits for the next events before it
answers, so the new draft is visible when it does.
`TODO.md` section 3 plans APPEND into every mailbox through Proton's
import API.

## How proton-bridge does it

- **STORE, COPY, MOVE** reach Proton through Gluon, Proton's IMAP
  library, which keeps the mailbox state in its own store. Labels are
  mailboxes there, so COPY to `Labels/x` adds a label and MOVE out of
  one removes it. On MOVE into a folder or system mailbox, a feature
  flag lets it skip the unlabel call (Proton has moved the message
  already); it keeps the call by default, as removing it broke clients
  (connector.go, BRIDGE-451).
- **EXPUNGE** is the same as peroxide's (peroxide copied it): unlabel,
  then delete for good from Trash and Drafts only what no other shown
  label holds; All Mail refuses.
- **APPEND** imports the message into any mailbox (Proton's import
  API), except All Mail; peroxide doesn't have that client yet.
- proton-bridge also refuses MOVE between INBOX and Sent, and APPEND
  or MOVE into All Mail or Scheduled. peroxide doesn't check these.
