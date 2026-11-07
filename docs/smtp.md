# Sending mail: the SMTP server and `peroxide sendmail`

## Connecting

The SMTP server (go-smtp, package `smtp`) listens on `127.0.0.1:1025`
by default, as `localhost`. It offers `AUTH PLAIN` only: the username is
your Proton username, the password the bridge password. A client that
knows only `AUTH LOGIN` can't log in. Without `-tls-cert`, it accepts
the password over the plain connection, as it never leaves the machine;
with it, the whole connection is TLS (implicit, port 465 style, not
STARTTLS). See [commands.md](commands.md#listening-and-tls).

MAIL FROM is ignored: the sender is the message's `From:` header. Each
RCPT TO is a recipient. peroxide sets no message size limit of its own;
Proton's limits apply when it uploads.

## What happens to a message

`smtp.SendMail`, step by step:

1. **Parse the header.** Exactly one `From:` address, and at least one
   recipient in `To`, `Cc` or `Bcc`, or the message is refused.
   Recipients given by RCPT TO (or on the `sendmail` command line) that
   are in neither To nor Cc become Bcc, unless the message has a `Bcc:`
   header, which then wins.
2. **Choose the sender.** The From address must be one of your Proton
   addresses (compared without case), with a key peroxide has unlocked;
   otherwise "unknown sender address" or "sender address key hasn't been
   decrypted". The address list is the one read when the SMTP client
   logged in.
3. **Find the parent.** If `In-Reply-To` names a message in Proton (by
   its Message-Id), the draft is created as a reply to it, so Proton
   marks the parent replied and threads the conversation.
4. **Create a draft** in Proton (`POST /messages`) with the subject,
   recipients, your From name, the original header and the Message-Id,
   and an empty body encrypted to your own key.
5. **Upload the attachments.** For each MIME part with an attachment
   disposition and a filename: a new session key, the data encrypted
   with it (not signed), uploaded to `/attachments`, its key kept for
   the send. A part's `Content-Id` is kept.
6. **Store the body.** The first inline part becomes the body, and a
   later `text/html` one replaces it (so HTML wins over its text/plain
   alternative); other inline parts are dropped. It is encrypted to,
   and signed with, your key and saved in the draft. A message with no
   inline part is refused ("message doesn't contain a body part").
7. **Look up every recipient's key** (`GET /keys?Email=...`). Proton
   answers with the keys it knows for that address: a Proton user's,
   and possibly others it has. peroxide uses the first key returned,
   if any.
8. **Send** (`POST /messages/<id>`), with up to two packages:
   - recipients with a key: the body's session key encrypted to each
     recipient's key (Proton's "internal" scheme), signed;
   - recipients without: the session key in clear for Proton to
     encrypt-at-rest and deliver as normal mail, unsigned.
9. Proton sends, and files the message in **Sent**. Don't have your
   client also save a copy (see [imap/changes.md](imap/changes.md#append)).

SMTP answers `250` when Proton accepts the send, or `554` with the
error (the text is peroxide's or Proton's message). A failure after
step 4 leaves the draft in Drafts: delete it, or open it in Proton's
web app and send it from there.

### What it doesn't do

- **No PGP/MIME or inline PGP to external recipients.** Every
  recipient with a key gets the "internal" package, Proton's scheme for
  mail between Proton users. Proton's contact settings (encrypt, sign,
  scheme) are not read.
- **No signature for unencrypted recipients**, and attachments are
  never signed.
- Only the first key of a recipient is used, whatever its `Send` flag.
- Only `In-Reply-To` sets the parent, as a reply: forwards are sent as
  new messages.
- No duplicate check: a client that retries a send (after a timeout)
  sends twice.
- Inline parts other than the body (an inline image without an
  attachment disposition, a text/plain alternative) are dropped.

## `peroxide sendmail`

`peroxide sendmail <username> -- <recipients...>` reads one message on
stdin and sends it through the same steps, logging in for this one
message (it asks for the bridge password, or reads
`PEROXIDE_BRIDGE_PASS`). For `git send-email`:

    [sendemail]
        sendmailcmd = peroxide sendmail you@proton.me --

It reads stdin to the end: `-i` (which git send-email always passes) is
accepted and changes nothing, and no
other sendmail flag is understood. Each run logs in and unlocks your
keys afresh, as no session outlives the command.

## How proton-bridge does it

proton-bridge (internal/services/smtp) builds **send preferences** per
recipient from the key lookup and your contact settings: Proton
recipients get the internal scheme; external recipients with a key get
PGP/MIME or inline PGP as the contact says; others get clear or
clear-signed MIME. It keeps the message's MIME structure and all its
parts. It keeps a hash of each message sent in the last 90 seconds and
skips a duplicate send. It adds `AUTH LOGIN` beside go-smtp's `AUTH
PLAIN` (internal/services/imapsmtpserver/smtp.go).

peroxide's send path is hydroxide's, nearly unchanged; it covers mail
between Proton users and plain mail to others. `TODO.md` section 3
step 3 notes the duplicate question.
