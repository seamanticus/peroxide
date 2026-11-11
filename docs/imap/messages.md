# Messages: FETCH, decryption and what can't be decrypted

## What Proton gives peroxide

Proton doesn't keep the MIME message that arrived. It keeps:

- **metadata**: subject, sender, recipients, date, size, flags, labels,
  attachment list (listed in pages, cheap, what the local database
  holds);
- the **body**: one part (text/plain or text/html), as an armored
  OpenPGP message encrypted to one of your address keys; for mail
  received as PGP/MIME, the whole decrypted MIME message;
- the **original header**, as received, but only in the full message
  (`GET /messages/<id>`), not in listings;
- each **attachment**: its key packets (the session key encrypted to
  your key) with the metadata, and its encrypted data, downloaded on
  its own (`GET /attachments/<id>`).

peroxide builds a MIME message from these **once**, stores it, sealed,
and serves every FETCH of it from what is stored (see
[Where messages are kept](#where-messages-are-kept)). What a client is
served never changes under a UID: when it must change (an edited
draft, a key unlocked since), the message gets a new UID
([Messages that change](#messages-that-change)).

## What each FETCH item costs

| Item | Source | Request |
|---|---|---|
| `FLAGS`, `UID`, `INTERNALDATE` | local database | none |
| everything else: `ENVELOPE`, `RFC822.SIZE`, `BODYSTRUCTURE`, `BODY`, `BODY[...]`, `RFC822...` | the stored message | none once stored; else one, plus one per attachment, to build and store it |

After login, once the message list is downloaded
([sync.md](sync.md)), peroxide downloads every message in the
background, newest first, so most FETCHes find it stored; the mailbox a
client last selected goes first. A FETCH of messages not stored yet
builds and stores them first, up to 64 at a time in parallel, paced
with the background download (Proton asking either to slow down slows
both); the response starts once the first 64 are ready. A FETCH of a
message being downloaded waits for that download rather than making a
second. Each such message still costs one request (and one per
attachment): Proton has no way to ask for several full messages at
once.

`FETCH ALL`, `FAST` and `FULL` are expanded as RFC 3501 defines. An
item peroxide doesn't know fails the whole FETCH (it isn't dropped
silently). A body part the message doesn't have (`BODY[7]` of a message
with two) is an empty string, as Dovecot and gluon answer.

A message Proton deleted after it was listed, and before its delete
event arrived, is skipped: the FETCH answers for the others, and the
EXPUNGE follows at the next poll. Any other failure fails the FETCH;
one that isn't Proton asking to slow down, a server error or the
network also puts the message in the [failed set](#failed-messages).

## \Seen

`BODY[...]`, `RFC822` and `RFC822.TEXT` mark an unread message read, as
RFC 3501 says; `BODY.PEEK[...]` and `RFC822.HEADER` don't. In a mailbox
opened with EXAMINE (read-only), nothing marks a message read.

peroxide marks the messages read after the FETCH response is written,
in one request for all of them, and then sends `FETCH (FLAGS (\Seen))`.
If that request fails, the FETCH still answers OK (you have the
messages), the message stays unread, and the failure is logged.

## How a message is built

In package `internal/msgbuild` (`Builder.Build`), which the IMAP server
calls; the server then derives BODYSTRUCTURE and ENVELOPE from the bytes
(`internal/imap/structure.go`). As proton-bridge builds it
(pkg/message/build.go, `BuildRFC822Into`):

- **With attachments**:

      multipart/mixed; boundary=<sha1 of the message ID>
        1. the body: text/plain or text/html; charset=utf-8,
           quoted-printable, decrypted
        2..n. each attachment: its MIME type, base64, decrypted,
           Content-Disposition: attachment; filename=...

  An attached message (`message/rfc822`) has no transfer encoding, so
  that its BODYSTRUCTURE carries its envelope and structure.
- **Received as PGP/MIME** (Proton's `MIMEType` is `multipart/mixed`:
  the decrypted body is a whole MIME message): that message, its header
  fields joined to the message's. If the sender signed it, it is served
  as `multipart/signed` (RFC 3156), the signed part byte for byte and
  the signature as `OpenPGP_signature.asc`, so that your client can
  verify it.
- **The Sent copy of mail your client encrypted** (a text draft or sent
  message whose body, signed by your own address key, starts with
  peroxide's `X-Peroxide-Body:` line): the `multipart/encrypted`
  message your client sent, its header fields joined to the message's
  ([../encrypt.md](../encrypt.md#the-sent-copy-of-mail-your-client-encrypted)).
  It has no `X-Peroxide-Signature` field.
- **Otherwise**: a `multipart/mixed` of the body alone.

The build is deterministic: building a message again gives the same
bytes, unless what Proton has or the keys peroxide unlocked changed.

**The header** (`BODY[HEADER]`) starts from Proton's copy of the
original header, so `Date`, `Message-Id`, `In-Reply-To`, `References`,
`List-*` and the rest are what the sender sent. On top of it peroxide
sets `Subject` and `From` from Proton's metadata, and `To`, `Cc`, `Bcc`
and `Reply-To` when the metadata has any (otherwise the original's
stay), and its own `Content-Type` and `MIME-Version`; it drops the
original `Content-Transfer-Encoding`. A missing or unparseable `Date`
becomes Proton's received time, a missing `Message-Id`
`<id@protonmail.com>`. If the original header can't be parsed at all,
it is ignored (logged) and the header is built from the metadata alone.

**ENVELOPE** comes from that header (RFC 3501 section 7.4.2): `Date`,
`Sender`, `In-Reply-To` and `Message-Id` are the original's; `Sender`
and `Reply-To` default to `From`. A message with no sender has `From`
NIL.

**BODYSTRUCTURE**, the parts and **sizes** are computed from the stored
bytes by go-imap's parser, so they always match what `BODY[]` sends:
`RFC822.SIZE` is the length of `BODY[]`, a part's size its encoded
size.

**HEADER.FIELDS** and **HEADER.FIELDS.NOT** send only the fields named,
or all but those, matched without regard to case.

**Signatures** of the body and attachments are checked by peroxide and
the verdict written in the header ([Signatures](#signatures)); a
PGP/MIME signature is also handed to your client (above), which can
check it itself.

**Bytes that can't be parsed.** If a built message can't be parsed
(malformed MIME inside a PGP/MIME body, say), it is served instead as a
note saying why, with the message as built attached as `message.eml`:
nothing is lost, and the log names it.

## SEARCH

SEARCH reads what is stored and downloads nothing. A message stored is
searched as served: `HEADER` and `TEXT` in its header (original fields
included), `BODY` and `TEXT` in its text parts decoded (attachments,
`.pgp` files included, are left out), `SENTBEFORE` and `SENTSINCE`
against its `Date` header, `LARGER` and `SMALLER` against its size. A
message not stored yet is searched in its metadata: the header fields
peroxide builds from it, Proton's received time and size, and no body
(`BODY` doesn't match it). So results can grow while the background
download runs.

## Mail peroxide can't decrypt

Some mail can't be decrypted with the keys peroxide unlocked. peroxide
never fails a FETCH for it, and never drops it. What you get depends
on why.

### Encrypted end to end to a key Proton doesn't have

Someone sent you mail encrypted with OpenPGP to a key you keep
elsewhere (your GnuPG key), not to your Proton key. Proton stored it as
received. Only your own client can read it.

**Without attachments**, peroxide hands it over unchanged as PGP/MIME
(RFC 3156):

    multipart/encrypted; protocol="application/pgp-encrypted"
      1. application/pgp-encrypted: "Version: 1"
      2. application/octet-stream: the armored PGP message, byte for byte

with the original header, and a real BODYSTRUCTURE of that. aerc (with
its gpg or internal provider), Thunderbird, Evolution and mutt decrypt
it with your key as they would any PGP/MIME mail. The log says
`Message <id> is encrypted to a key Proton doesn't have: passed through
as PGP/MIME` when it is built.

peroxide tells this case apart without decrypting anything: the body's
key packets name no key of the account, locked or not. Mail encrypted
to a hidden recipient (key ID 0) names no key either, and is treated
the same way.

**With attachments**, multipart/encrypted has no room for them, so the
message is built as usual, with the body served armored (next
section).

### A body or attachment no unlocked key opens

The body is encrypted to a key of your account that peroxide didn't
unlock (one added since peroxide started, see
[../accounts.md](../accounts.md#keys-that-change-while-peroxide-runs)),
or the decryption failed for another reason (corruption, an integrity
check failure at the end of the data). Or an attachment's key packets
open with no unlocked key, or its data fails to decrypt.

- The **body** is served as a text part (its own text type, else
  `text/plain`), its content the armored PGP message as stored, with an
  armor `Comment:` line saying why, e.g. `Comment: This message could
  not be decrypted: encrypted to a key of the account that peroxide
  hasn't unlocked (one added since it started?)`.
- An **attachment** is served as `application/octet-stream` named
  `<original name>.pgp`, its content the armored PGP message (its key
  packets, then its data), with `Comment: This attachment could not be
  decrypted: ...` and the reason.

You can save either and decrypt it yourself (`gpg -d`) if you have the
key. The log names each one when the message is built.

Body and attachments are decrypted whole before anything is built, so
an error at their very end (the integrity check) still leads to the
armored fallback, not to a truncated message.

## Messages that change

A UID names one content forever (RFC 3501 section 2.3.1.1). Two things
can change what peroxide would build:

- **An edited draft** (or any message Proton reports as updated, not
  just its flags or labels): if it is stored, peroxide builds it again
  and compares the bytes. The same: nothing happens. Different: the
  message gets a new UID in every mailbox it is in, and clients are
  told `EXPUNGE` of the old one and `EXISTS` of the new, as gluon does.
  A message not stored yet is simply built as Proton has it at its
  next FETCH. Several updates of one draft in an events page (the web
  client autosaving) build it once, after the page is applied; the
  download of the draft holds no lock, so other events, STOREs and the
  background download don't wait for it. If Proton can't be reached,
  the stored version stays until the draft's next update.
- **A key unlocked since** the messages were built: at login, if the
  unlocked keys differ from those the stored messages were built with,
  every message stored with something left encrypted (armored body,
  `.pgp` attachment, PGP/MIME pass-through) is built again, and gets a
  new UID if it now decrypts.

## Signatures

Mail between Proton users, and mail a sender signed inside OpenPGP
encryption, carries the sender's signature inside the encrypted body.
It signs the text, which peroxide decrypts and serves in the clear:
your client gets no signature to check, whichever client it is. So
peroxide checks it when it builds the message, with the sender's public
keys as Proton publishes them (`GET /keys`, the keys Proton's own web
client checks against), and writes the result in the message header:

    X-Peroxide-Signature: pass header.from=alice@proton.me key=4F2A...9C1D

The keys of each sender are fetched once and kept for a day, so only
the first signed message of a sender costs a request. A message that
isn't signed gets no field. Mail you sent, signed with your own key,
is checked with it.

Proton's clients also sign each **attachment**, apart: a detached
signature over the file, kept beside it. peroxide checks that too, over
the decrypted file, with the same keys and no further request, and
writes the result twice: in the attachment part's own header, and as
a summary on the message's field (`attachments=`). Attachments sent
through peroxide's SMTP server are signed the same way
([../smtp.md](../smtp.md)).

### The field, for client authors

The field is written once, when peroxide builds the message, and never
changes under the message's UID. It follows RFC 8601's
`Authentication-Results` result values (section 2.7.1) and its
`ptype.property=value` shape:

    X-Peroxide-Signature: <result> [(<reason>)] [header.from=<address>] [key=<fingerprint>] [attachments=<result>]

| Result | Meaning |
|---|---|
| `pass` | The signature verifies with a key Proton publishes for the sender (`header.from`), the one in `key`: the text is as the sender signed it |
| `fail` | Made by the key in `key`, which the sender publishes, but it doesn't verify: the text changed after it was signed. `reason` says how |
| `neutral` | Made by a key the sender doesn't publish (an old key, or someone else's): it can't vouch for the sender |
| `permerror` | Can't be checked: Proton publishes no key for the sender (most senders outside Proton), only keys their owner marked compromised, or refused to say which |
| `none` | The body isn't signed, but an attachment is (see `attachments`) |
| (no field) | Neither the body nor any attachment is signed, or the body is passed through still encrypted, or couldn't be decrypted |

- `header.from` is the sender address from Proton's metadata, the
  `From` peroxide writes; `key` is the signing key's fingerprint in
  upper-case hex (V4: 40 digits, V6: 64).
- `reason` is free text for people; don't parse it.
- Only peroxide writes the field: one in the header the sender sent,
  or in a PGP/MIME message's own header, is removed before peroxide
  adds its own. A client can trust the field as far as it trusts the
  connection to peroxide.
- `pass` says the sender's Proton key made the signature, as Proton's
  web client does with its "verified" mark; it says nothing about
  whether that key is the one you expect (Proton's key pinning in
  contacts isn't read). Show `fail` as a warning; `neutral` and
  `permerror` as "not verified".
- Received PGP/MIME (`multipart/signed`) gets the field too: a client
  that verifies such mail itself can ignore it.

**Attachments.** The message's field carries `attachments=` when the
message has attachments:

| `attachments=` | Meaning |
|---|---|
| `pass` | Every attachment is signed and verifies with the sender's key |
| `fail` | At least one is signed by the sender's key and doesn't verify: the file changed |
| `permerror` | At least one can't be checked (no fail) |
| `neutral` | At least one is signed by a key the sender doesn't publish, or isn't signed while others are (no fail, no permerror) |
| `none` | No attachment is signed (mail from outside Proton, mostly) |

Each decrypted attachment that is signed also carries the field in its
own part header, `X-Peroxide-Signature: <result> [(<reason>)]
[key=<fingerprint>]`, with the same results and no `header.from` (the
signer is the message's sender); fetch it with `BODY.PEEK[<n>.MIME]` to
say which file failed. An attachment that isn't signed, or is served
still encrypted (`.pgp`), has none. A part's field, too, is written
only by peroxide: it builds those headers itself from Proton's
attachment list. That holds for the attachment parts of a
`multipart/mixed` message peroxide built, its direct parts 2 and
after; the parts inside a received PGP/MIME message are the sender's,
byte for byte, and a field among them isn't peroxide's: ignore it.

A client wanting to show the verdict reads it with the rest of the
header (`BODY.PEEK[HEADER.FIELDS (X-PEROXIDE-SIGNATURE)]`, or the whole
header it fetches anyway).

Messages stored before peroxide checked signatures have no field until
they are built again (edited, a key change, or `download-mailbox
-discard-local-copy`); those stored before it checked attachments have
no `attachments=`.

## Where messages are kept

Each message is stored as served, sealed with AES-256-GCM, one file per
message under `$XDG_DATA_HOME/peroxide/<user>/` (or
`~/.local/share/peroxide/<user>/`), named by the hex of Proton's message
ID; its BODYSTRUCTURE, envelope and size are kept, sealed with the same
key, in the account's database. The key is derived from your bridge
password and never written: [../security.md](../security.md#the-message-store).
A message deleted in Proton is removed from the store with it.

### Failed messages

A message whose download or build fails for a reason that retrying soon
won't change (Proton refusing it, say) is kept in a set of failed
messages, as proton-bridge does. The background download skips it;
any FETCH of it, any event about it, and `peroxide download-mailbox`
try again, and success takes it out. It stays listed, with its flags,
meanwhile.

## How proton-bridge does it

proton-bridge builds the message the same way from the same pieces
(pkg/message/build.go), downloads every message at login (its sync's
download and build stages) and keeps the result in gluon's store, so a
message is downloaded and decrypted once, ever. It adds `X-Pm-*`
headers (`X-Pm-Internal-Id`, `X-Pm-External-Id`, `X-Pm-Date`), which
peroxide doesn't. Its multipart/signed rewrites the signed part's
header, and names the hash `SHA-256` in `micalg`; peroxide keeps the
signed part byte for byte and writes `pgp-sha256` (RFC 3156), so the
signature verifies. bridge checks no signature inside an encrypted
body, so its clients see none; peroxide writes `X-Peroxide-Signature`.
An edited draft is built again and replaces the
stored one under a new UID only if the bytes differ
(`applyMessageUpdated`); peroxide does the same. Messages that fail
are kept in its sync status (`FailedMessages`) and not retried by the
sync; peroxide also retries them on FETCH, events and
`download-mailbox`.

For a body it can't decrypt, proton-bridge serves the PGP/MIME
pass-through only for messages without attachments
(`buildPGPMIMEFallbackRFC822`), and otherwise armored text and
`.pgp` attachments with a comment (`writeCustomTextPart`,
`writeCustomAttachmentPart`). peroxide follows that, and in addition
passes through only mail encrypted to a key you don't hold in Proton:
mail encrypted to a Proton key peroxide hasn't unlocked yet is shown
armored, as your own client couldn't open it either, and is built
again once that key is unlocked.
