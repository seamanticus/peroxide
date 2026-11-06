# Messages: FETCH, decryption and what can't be decrypted

## What Proton gives peroxide

Proton doesn't keep the MIME message that arrived. It keeps:

- **metadata**: subject, sender, recipients, date, size, flags, labels,
  attachment list (listed in pages, cheap, what the local database
  holds);
- the **body**: one part (text/plain or text/html), as an armored
  OpenPGP message encrypted to one of your address keys;
- the **original header**, as received, but only in the full message
  (`GET /messages/<id>`), not in listings;
- each **attachment**: its key packets (the session key encrypted to
  your key) with the metadata, and its encrypted data, downloaded on
  its own (`GET /attachments/<id>`).

peroxide rebuilds a MIME message from these every time a client asks.

## What each FETCH item costs

| Item | Source | Request |
|---|---|---|
| `FLAGS`, `UID`, `INTERNALDATE`, `RFC822.SIZE`, `ENVELOPE` | local database | none |
| `BODYSTRUCTURE`, `BODY` | full message | one per message, then cached |
| `BODY[HEADER]`, `BODY[HEADER.FIELDS (...)]`, `RFC822.HEADER` | full message | one per message, then cached |
| `BODY[]`, `BODY[TEXT]`, `RFC822`, `BODY[n]` | full message, plus each attachment | one, plus one per attachment |

The full message (encrypted body, original header, attachment list) is
kept in memory for the 32 messages used most recently per account.
Attachments are downloaded every time. A client listing a folder with
BODYSTRUCTURE (aerc does) makes one request per message the first time.
`TODO.md` section 9 describes the plan to make those free.

`FETCH ALL`, `FAST` and `FULL` are expanded as RFC 3501 defines. An
item peroxide doesn't know fails the whole FETCH (it isn't dropped
silently).

A message Proton deleted after it was listed, and before its delete
event arrived, is skipped: the FETCH answers for the others, and the
EXPUNGE follows at the next poll. Any other failure fails the FETCH.

## \Seen

`BODY[...]`, `RFC822` and `RFC822.TEXT` mark an unread message read, as
RFC 3501 says; `BODY.PEEK[...]` and `RFC822.HEADER` don't. In a mailbox
opened with EXAMINE (read-only), nothing marks a message read.

peroxide marks the messages read after the FETCH response is written,
in one request for all of them, and then sends `FETCH (FLAGS (\Seen))`.
If that request fails, the FETCH still answers OK (you have the
messages), the message stays unread, and the failure is logged.

## How a message is rebuilt

    multipart/mixed; boundary=<sha1 of the message ID>
      1. the body: text/plain or text/html; charset=utf-8,
         quoted-printable, decrypted
      2..n. each attachment: its MIME type, base64, decrypted,
         Content-Disposition: attachment; filename=...

Every message is `multipart/mixed`, even one without attachments.

**The header** (`BODY[HEADER]`) starts from Proton's copy of the
original header, so `Date`, `Message-Id`, `In-Reply-To`, `References`,
`List-*` and the rest are what the sender sent. On top of it peroxide
sets `Subject` and `From` from Proton's metadata, and `To`, `Cc`, `Bcc`
and `Reply-To` when the metadata has any (otherwise the original's
stay), and its own `Content-Type` (the multipart above) and
`MIME-Version`; it drops the original `Content-Transfer-Encoding`. A
missing or unparseable `Date` becomes Proton's received time, a missing
`Message-Id` `<id@protonmail.com>`. If the original header can't be
parsed at all, it is ignored (logged) and the header is built from the
metadata alone. The listing that fills the local database carries no
original header, which is why only body sections and BODYSTRUCTURE,
not ENVELOPE, use it.

**ENVELOPE** is built from metadata only, not from the original header:
its `Date` is when Proton received the message, its `Message-Id` is the
original one (or `<id@protonmail.com>`), its `In-Reply-To` is empty and
its `Sender` is always `From` (`TODO.md` section 9, step 8). A message
with no sender has `From` NIL.

**HEADER.FIELDS** and **HEADER.FIELDS.NOT** send only the fields named,
or all but those, matched without regard to case.

**Sizes.** `RFC822.SIZE` is Proton's size of the message as stored,
not of what peroxide builds; BODYSTRUCTURE gives an attachment's
decrypted size, not its base64 size. Clients that trust these to
allocate or to show progress may be slightly off (`TODO.md` section
11).

**Signatures** are not checked: neither the body's nor attachments'.

**SEARCH** runs on the local database: flags, dates (Proton's received
time, for SENTBEFORE and SENTSINCE too), sizes, and the header fields
peroxide builds from metadata. `SEARCH BODY` and `SEARCH TEXT` match
against the body and original header, which the local database doesn't
hold (listings carry neither): they match nothing. Search those in
Proton's web app, or in a client that downloads the messages.

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
as PGP/MIME` at every BODYSTRUCTURE.

peroxide tells this case apart without decrypting anything: the body's
key packets name no key of the account, locked or not. Mail encrypted
to a hidden recipient (key ID 0) names no key either, and is treated
the same way.

**With attachments**, multipart/encrypted has no room for them, so the
message is rebuilt as usual, with the body served armored (next
section).

### A body or attachment no unlocked key opens

The body is encrypted to a key of your account that peroxide didn't
unlock (one added since peroxide started, see
[../accounts.md](../accounts.md#keys-that-change-while-peroxide-runs)),
or the decryption failed for another reason (corruption, an integrity
check failure at the end of the data). Or an attachment's key packets
open with no unlocked key.

- The **body** is served as a text part (its own text type, else
  `text/plain`), its content the armored PGP message as stored, with an
  armor `Comment:` line saying why, e.g. `Comment: This message could
  not be decrypted: encrypted to a key of the account that peroxide
  hasn't unlocked (one added since it started?)`.
- An **attachment** is served as `application/octet-stream` named
  `<original name>.pgp`, its content the armored PGP message (its key
  packets, then its data), with `Comment: This attachment could not be
  decrypted: no key peroxide has unlocked opens it`.

You can save either and decrypt it yourself (`gpg -d`) if you have the
key. The log names each one, at every fetch.

The body is decrypted whole before anything is sent, so an error at its
very end (the integrity check) still leads to the armored fallback,
not to a truncated message.

## How proton-bridge does it

proton-bridge builds the message the same way from the same pieces
(pkg/message/build.go), but keeps the result in its local store, so
a message is downloaded and decrypted once, ever. It adds `X-Pm-*`
headers (`X-Pm-Internal-Id`, `X-Pm-External-Id`, `X-Pm-Date`) and can
rebuild a signed message as multipart/signed
(`writeMultipartSignedRFC822`).

For a body it can't decrypt, proton-bridge serves the PGP/MIME
pass-through only for messages without attachments
(`buildPGPMIMEFallbackRFC822`), and otherwise armored text and
`.pgp` attachments with a comment (`writeCustomTextPart`,
`writeCustomAttachmentPart`). peroxide follows that, and in addition
passes through only mail encrypted to a key you don't hold in Proton:
mail encrypted to a Proton key peroxide hasn't unlocked yet is shown
armored, as your own client couldn't open it either.
