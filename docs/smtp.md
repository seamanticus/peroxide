# Sending mail: the SMTP server and `peroxide sendmail`

## Connecting

The SMTP server (go-smtp, package `smtp`) listens on `127.0.0.1:1025`
by default, as `localhost`. It offers `AUTH PLAIN` only: the username is
your Proton username, the password the bridge password. A client that
knows only `AUTH LOGIN` can't log in. The connection is TLS from its
first byte (implicit, port 465 style, not STARTTLS), with peroxide's
own certificate unless `serve` is given one; with `serve -plaintext`,
loopback only, the password is accepted over plain text. See
[commands.md](commands.md#listening-and-tls).

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
   with it and signed with your key apart (a detached signature over
   the file, uploaded beside it, as Proton's own clients do, so a
   Proton recipient can check it), uploaded to `/attachments`, its key
   kept for the send. A part's `Content-Id` is kept.
6. **Store the body.** The first inline part becomes the body, and a
   later `text/html` one replaces it (so HTML wins over its text/plain
   alternative); other inline parts are dropped. It is encrypted to,
   and signed with, your key and saved in the draft. A message with no
   inline part is refused ("message doesn't contain a body part").
7. **Look up every recipient's key** (`GET /keys?Email=...`). Proton
   answers with the keys it knows for that address: a Proton user's,
   and possibly others it has. peroxide uses the first key Proton marks
   active (not obsolete). A recipient whose keys are all obsolete makes
   the send fail, naming them, rather than getting in clear what they
   expect encrypted (as proton-bridge refuses too).
8. **Send** (`POST /messages/<id>`), with up to two packages:
   - recipients with a key: the body's session key encrypted to each
     recipient's key (Proton's "internal" scheme), signed;
   - recipients without: as your Proton setting **Sign external
     messages** says (read with `GET /mail/v4/settings` at each such
     send). On: the whole message as you wrote it, attachments
     included, but for its `Bcc`, signed with your key, which Proton
     delivers as PGP/MIME `multipart/signed` (RFC 3156): the recipient's
     client can check your signature over the text and every file, and
     a client without OpenPGP shows a `signature.asc` attachment. Off:
     the body and attachments as normal mail, unsigned. Either way the
     session key goes to Proton in clear, for it to deliver and encrypt
     at rest.
9. Proton sends, and files the message in **Sent**. A client that also
   saves its copy there (APPEND) gets the sent message, not a second
   one (see [imap/changes.md](imap/changes.md#append)).

SMTP answers `250` when Proton accepts the send, or `554` with the
error (the text is peroxide's or Proton's message). A failure after
step 4 leaves the draft in Drafts: delete it, or open it in Proton's
web app and send it from there.

### Mail your client signed or encrypted (PGP/MIME)

A message your client already secured with OpenPGP, as aerc does with
`:sign` and `:encrypt` (`s` and `x` on the review screen), is a
`multipart/signed` (protocol `application/pgp-signature`) or
`multipart/encrypted` (protocol `application/pgp-encrypted`) message,
RFC 3156. Taking it apart, as steps 5 and 6 do, would break it, so
peroxide handles it apart (`internal/smtp/secured.go`):

- **Encrypted (and signed inside, or not):** sent whole. Steps 1 to 4
  are the same; the draft then holds your client's message as it came,
  as text, and that is what **Sent** shows (Proton takes only
  `text/plain` and `text/html` draft bodies). Your client's ciphertext
  goes to Proton as a PGP/MIME package (scheme 16): the recipient's key
  packet and the data packet, both as your client wrote them. Proton
  reassembles them into the
  `multipart/encrypted` message your client built. peroxide never sees
  the plaintext and needs neither the recipient's key nor the session
  key. The recipient decrypts it, and a signature inside verifies by
  your own key, the one your client signed with. See the limits below.
- **Signed only:** your client's signature is dropped (see why below):
  the signed part is sent as the message, through steps 1 to 7, signed
  by your **Proton address key** for every recipient whatever "Sign
  external messages" says. **Sent** shows it as an ordinary message. An
  external recipient gets a valid PGP/MIME signature by the address
  key; the body and its headers are what your client signed. A Proton
  user gets it
  as any mail between Proton users: encrypted to their key and signed
  inside by your address key, with no `multipart/signed` part, so a
  client shows it as unsigned. peroxide on their side checks that
  signature and reports it in the `X-Peroxide-Signature` header
  ([messages](imap/messages.md)).

#### Limits on a message your client encrypted

Proton's send API sets them; peroxide refuses such a send before it
creates a draft, with a message saying why. Seen on 2026-11-11:

- **Proton users can't receive it.** Proton delivers to its own users
  only its internal package, with a `text/plain` or `text/html` body:
  a PGP/MIME package for them is refused (`2001 Wrong type of package
  for internal recipient`), and so is an internal one holding PGP/MIME
  (`2001 Invalid package type`). Send to them without encrypting in
  your client: peroxide encrypts the message to their Proton key, end
  to end, as for any mail between Proton users.
- **One key packet per recipient.** Proton refuses more
  (`2001 Multiple packets present`), and peroxide can't tell whose key
  an unknown packet is for. So the message goes to one recipient
  without a Proton key at a time, and your client must not encrypt to
  any other key Proton doesn't know: with aerc, turn
  `pgp-self-encrypt` off, unless it encrypts to your Proton address key,
  whose packet peroxide leaves out.

#### Why a signed-only message loses your signature

This is how Proton's send API works, not something peroxide can work
around. The only way to have Proton deliver a `multipart/signed`
message to an external recipient is its clear-signed MIME package
(ClearMIME, scheme 32): the client hands over the signed part with a
signature, and Proton builds the message, writing `signature.asc`
itself. Seen on 2026-11-11:

- A ClearMIME body signed only by your client's key is refused:
  `2001 Invalid or missing message signature`. Proton checks the
  signature against your address key.
- With the address key's signature added beside your client's, Proton
  accepts it but delivers only a signature of its own by the address
  key, made afresh. Your client's signature is dropped.

There is no package that sends a client's MIME unchanged and unencrypted.
So peroxide doesn't send your client's signature at all: it signs the
signed part with your address key, as Proton's own "Sign external
messages" does, and Proton delivers that.

The recipient checks that signature against your Proton public key
(Proton publishes it for lookup, and the web app's
**Settings > Encryption and keys** exports it). A client that only knows
your own key shows it as `Unknown` for the address key's ID.

To have the recipient see a signature by a key you control:

- **Encrypt as well as sign** (aerc: `x` and `s`). The ciphertext isn't
  opened by Proton, so the signature inside is your client's, untouched.
  This needs the recipient's public key in your keyring.
- **Sign with your Proton address key in your client.** Export the
  address's private key from Proton's web app (**Settings > Encryption
  and keys**), import it into GnuPG, and set aerc's `pgp-key-id` to it
  (aerc-accounts(5)). Proton's signature is then by the same key your
  client signs with. You keep a copy of the key outside Proton, and
  must protect it; when you rotate keys in Proton, export again.

### What it doesn't do

- **No PGP/MIME or inline PGP to external recipients** built by
  peroxide; only the client's own encrypted message (above) is passed
  on. Every
  recipient with a key gets the "internal" package, Proton's scheme for
  mail between Proton users. Proton's contact settings (encrypt, sign,
  scheme) are not read: a contact's own "sign" choice doesn't override
  the account setting.
- Attachments to recipients with a key are signed apart (above), but
  only a Proton recipient's client checks that signature.
- Only one key of a recipient is used, the first active one.
- Only `In-Reply-To` sets the parent, as a reply: forwards are sent as
  new messages.
- No duplicate check: a client that retries a send (after a timeout)
  sends twice.
- Inline parts other than the body (an inline image without an
  attachment disposition, a text/plain alternative) are dropped.

## `peroxide sendmail`

`peroxide sendmail <username> -- <recipients...>` reads one message on
stdin and sends it through the same steps. It asks for the bridge
password, or reads `PEROXIDE_BRIDGE_PASS`. For `git send-email`:

    [sendemail]
        sendmailcmd = peroxide sendmail you@proton.me --

It reads stdin to the end: `-i` (which git send-email always passes) is
accepted and changes nothing, and no other sendmail flag is understood.
At least one recipient is required: they are the SMTP envelope's
recipients, and the message's To and Cc aren't read for them.

While `peroxide serve` runs, `sendmail` doesn't log in itself: that
would spend the refresh token `serve` holds. It hands the message to
`serve`'s SMTP server, logged in with the username and bridge
password; give it the same `-smtp-host`, `-smtp-port`, and `-tls-cert`
or `-plaintext`,
as `serve` ([commands.md](commands.md#sendmail-username----recipients)).
If `serve` doesn't greet within 10 seconds, `sendmail` gives up and
asks whether `serve` and it agree about TLS: a plain client and a TLS
server, or the reverse, would otherwise each wait for the other. Once
`serve` has accepted the message, a failed `QUIT` is only a warning and
`sendmail` succeeds, so that `git send-email` doesn't send it again.

With no `serve` running, each run logs in and unlocks your keys
afresh, and the session ends with the command.

## How proton-bridge does it

proton-bridge (internal/services/smtp) builds **send preferences** per
recipient from the key lookup and your contact settings: Proton
recipients get the internal scheme; external recipients with a key get
PGP/MIME or inline PGP as the contact says; others get clear or
clear-signed MIME. It keeps the message's MIME structure and all its
parts. Its send path has no case for a message the client signed or
encrypted: it parses it into a body and attachments like any other
(its parser names unnamed parts `attachment.bin`), so the client's
signature or ciphertext doesn't survive either (read from the code,
not tried). It keeps a hash of each message sent in the last 90
seconds and skips a duplicate send. It adds `AUTH LOGIN` beside
go-smtp's `AUTH PLAIN` (internal/services/imapsmtpserver/smtp.go).

peroxide's send path is hydroxide's, nearly unchanged; it covers mail
between Proton users and plain mail to others. It keeps no record of
sends: APPEND finds a saved copy by its `Message-Id` instead
([imap/changes.md](imap/changes.md#append)).
