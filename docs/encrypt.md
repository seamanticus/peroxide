# Encryption and signatures

What you can expect when you sign or encrypt mail with OpenPGP in your
client, and what you get when others do. peroxide stands between your
client and Proton, and Proton's API decides most of what is possible:
the reasons are below the tables.

## Who holds which key

- **Your Proton address keys.** Proton keeps them, encrypted with your
  mailbox password; peroxide unlocks them at login. Proton publishes
  their public half for your address, so Proton users and many other
  senders encrypt to it. peroxide signs and decrypts with them.
- **Your own key** (GnuPG, the one your client uses), if you have one.
  Proton doesn't have it, unless you import it in Proton's web app
  (**Settings > Encryption and keys**). peroxide never has it.
- **The recipient's key.** For a Proton user, Proton has it and
  peroxide encrypts to it. For anyone else Proton usually has none; your
  client may have theirs in its keyring.

In aerc, `s` and `x` on the review screen sign and encrypt, and
`pgp-self-encrypt` (aerc-accounts(5)) also encrypts to your own key.
Other clients have the same three switches under other names.

## Sending to someone outside Proton

A recipient Proton has no key for: most addresses outside Proton.

| In your client | What the recipient gets | Your Sent copy |
|---|---|---|
| Nothing | Plain mail. If **Sign external messages** is on in Proton's settings, signed by your Proton address key (`multipart/signed`) | The message |
| Sign | Your client's signature is replaced by one from your **Proton address key** (`multipart/signed`). Their client shows it as unknown unless they have your Proton public key | The message, without a signature part |
| Encrypt (and sign, or not) | Your client's encrypted message, untouched: they decrypt it with their key, and your signature inside verifies with your own key | Your client's encrypted message. peroxide serves it as sent, but no one but the recipient can decrypt it; Proton's apps show it as text (see [The Sent copy](#the-sent-copy-of-mail-your-client-encrypted)) |
| Encrypt, with your own key too (`pgp-self-encrypt`) | **Refused** before anything is sent: Proton takes one key packet per recipient. Turn `pgp-self-encrypt` off (it works if your own key is your Proton address key) | Nothing |
| Encrypt, to several recipients | **Refused**: send to each apart | Nothing |

## Sending to a Proton user

Any recipient Proton has a key for: Proton users, and the rare outside
address whose key Proton knows.

| In your client | What the recipient gets | Your Sent copy |
|---|---|---|
| Nothing | Encrypted end to end to their Proton key, signed inside by your Proton address key. A client through peroxide shows no signature: peroxide's `X-Peroxide-Signature: pass` field says it verified | The message |
| Sign | The same as Nothing: your client's signature is replaced by the address key's, inside the encryption | The message, without a signature part |
| Encrypt (and sign, or not) | **Refused** before anything is sent: Proton can't deliver your client's encrypted message to its users. Send without encrypting in your client: peroxide encrypts it to their Proton key itself, end to end | Nothing |

A message to a Proton user and someone outside Proton at once follows
both tables: signed only, each gets their own row; encrypted by your
client, it is refused.

## Receiving

| What was sent to you | What your client gets through peroxide |
|---|---|
| From a Proton user | Decrypted, in the clear. The sender's signature is inside the encryption, so your client gets none to check; peroxide checks it with the sender's Proton key and writes `X-Peroxide-Signature: pass` (or `fail`, `neutral`) |
| PGP/MIME from outside, signed (`multipart/signed`), encrypted to your Proton key or not | The sender's message, with the signed part byte for byte and the signature as `OpenPGP_signature.asc`: your client checks it with the sender's key from its keyring. peroxide's field is usually `permerror`, as Proton publishes no key for most senders outside Proton |
| Encrypted to your own key, not your Proton key | Without attachments: passed through as the `multipart/encrypted` it was, and your client decrypts it with your key. With attachments: the body and each attachment as armored text and `.pgp` files, to decrypt yourself |
| Encrypted to a Proton key peroxide hasn't unlocked | Armored text with a `Comment:` saying why, built again once the key is unlocked |
| Plain mail | Plain mail, no field |

The details, and the field's exact format for client authors, are in
[imap/messages.md](imap/messages.md#signatures) and
[imap/messages.md](imap/messages.md#mail-peroxide-cant-decrypt).

## Why it works this way

### A message your client secured is handled apart

A message your client already secured with OpenPGP is a
`multipart/signed` (protocol `application/pgp-signature`) or
`multipart/encrypted` (protocol `application/pgp-encrypted`) message,
RFC 3156. peroxide's normal send
([smtp.md](smtp.md#what-happens-to-a-message)) takes a message apart
into a body and attachments, which would break it, so peroxide handles
it apart (`internal/smtp/secured.go`):

- **Encrypted (and signed inside, or not):** checked before the draft
  is created (the limits below), then sent whole. The draft's body is
  your client's message as text (see
  [The Sent copy](#the-sent-copy-of-mail-your-client-encrypted)), and
  the send is a PGP/MIME package (scheme 16): the recipient's key
  packet and the data packet, both as your client wrote them. Proton
  reassembles them into the `multipart/encrypted` message your client
  built. peroxide never sees the plaintext and needs neither the
  recipient's key nor the session key.
- **Signed only:** the signed part is sent as the message, through the
  normal steps, signed by your Proton address key for every recipient,
  whatever "Sign external messages" says; your client's signature is
  not sent ([why](#why-a-signed-only-message-loses-your-signature)).

### Limits on a message your client encrypted

Proton's send API sets them; peroxide refuses such a send before it
creates a draft, with a message saying why. Seen on 2026-11-11:

- **Proton users can't receive it.** Proton delivers to its own users
  only its internal package, with a `text/plain` or `text/html` body: a
  PGP/MIME package for them is refused (`2001 Wrong type of package for
  internal recipient`), and so is an internal one holding PGP/MIME
  (`2001 Invalid package type`).
- **One key packet per recipient.** Proton refuses more (`2001 Multiple
  packets present`), and peroxide can't tell whose key an unknown packet
  is for. So the message goes to one recipient without a Proton key at
  a time, and your client must not encrypt to any other key Proton
  doesn't know. A packet to one of your Proton keys is the exception:
  peroxide recognises it and leaves it out.

### Why a signed-only message loses your signature

The only way to have Proton deliver a `multipart/signed` message to an
outside recipient is its clear-signed MIME package (ClearMIME, scheme
32): the client hands over the signed part with a signature, and Proton
builds the message, writing `signature.asc` itself. Seen on 2026-11-11:

- A ClearMIME body signed only by your client's key is refused: `2001
  Invalid or missing message signature`. Proton checks the signature
  against your address key.
- With the address key's signature added beside your client's, Proton
  accepts it but delivers only a signature of its own by the address
  key, made afresh. Your client's signature is dropped.

There is no package that sends a client's MIME unchanged and
unencrypted. So peroxide doesn't send your client's signature at all:
it signs with your address key, as Proton's own "Sign external
messages" does. To a Proton user, mail is always encrypted and signed
inside, so there is no `multipart/signed` for a client to show.

The recipient checks that signature against your Proton public key
(Proton publishes it for lookup, and **Settings > Encryption and keys**
exports it). A client that only knows your own key shows it as unknown,
by the address key's ID.

To have an outside recipient see a signature by a key you control:

- **Encrypt as well as sign.** Proton doesn't open the ciphertext, so
  the signature inside is your client's, untouched. This needs the
  recipient's public key in your keyring.
- **Sign with your Proton address key in your client.** Export the
  address's private key from Proton's web app (**Settings > Encryption
  and keys**), import it into GnuPG, and set your client to sign with it
  (aerc: `pgp-key-id`). Proton's signature is then by the same key your
  client signs with. You keep a copy of the key outside Proton, and
  must protect it; when you rotate keys in Proton, export again.

### The Sent copy of mail your client encrypted

Proton files the draft it sent in **Sent**, and a draft can only hold a
`text/plain` or `text/html` body (`2011` for anything else). So peroxide
stores your client's whole message as the text of the draft, behind a
first line of its own:

    X-Peroxide-Body: the PGP/MIME message below was sent as is by your mail client; peroxide serves it over IMAP as sent

- **Through peroxide**, that copy is served as the `multipart/encrypted`
  message your client sent (`internal/msgbuild/sentcopy.go`). Your
  client shows it as encrypted mail, which it can't decrypt: it is
  encrypted to the recipient alone.
- **In Proton's web and mobile apps**, it is that line followed by the
  message source as text: its headers and the armored ciphertext.

peroxide honours the line only in a draft or sent message of a plain
text body signed by one of your own address keys, which only peroxide
and your own Proton session make. The same line in mail someone sent
you is just text. Sent copies stored before this was added stay as
text.

## Crypto peroxide does and doesn't do

- Decrypts with your unlocked keys (ProtonMail's go-crypto, an OpenPGP
  implementation); serves mail it can't decrypt as armored text or
  PGP/MIME, never as a guess.
- Checks the signature of every signed message it builds against the
  sender's keys as Proton publishes them, and reports it in
  `X-Peroxide-Signature`. It doesn't check that those keys are the right
  ones: it trusts Proton's key server, and Proton's key pinning in
  contacts isn't read.
- Encrypts and signs mail to Proton users end to end. Mail to anyone
  else goes out as Proton sends it: plain, or signed by your address
  key, with the session key handed to Proton, which encrypts it at rest.
- Never builds PGP/MIME or inline PGP encryption for outside recipients
  itself, and doesn't read Proton's contact settings (encrypt, sign,
  scheme). Your client's own encryption is passed on, as above.

## How proton-bridge does it

On sending, proton-bridge encrypts to outside recipients whose key is
in your contacts, and has no case for a message the client signed or
encrypted ([smtp.md](smtp.md#how-proton-bridge-does-it)). On receipt,
it checks no signature inside an encrypted body, and passes mail it
can't decrypt through as PGP/MIME when it has no attachments, as
peroxide does ([imap/messages.md](imap/messages.md#how-proton-bridge-does-it)).
