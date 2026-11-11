# Security

What peroxide protects, against whom, and where its limits are. The
details are on the pages linked.

## Trust boundaries

    you --password once--> peroxide auth --SRP--> Proton
    client --bridge password--> peroxide (loopback) --TLS--> Proton

- **Proton's API** is reached over HTTPS only (`-api-endpoint`), with Go's
  TLS and system roots. peroxide trusts Proton's answers: which keys a
  recipient has, what a message contains.
- **Your password** is used only by `peroxide auth`. It never goes to
  Proton (SRP sends a proof), but peroxide stores it, sealed (below).
- **Clients** reach peroxide on loopback by default. They prove
  themselves with the bridge password. Any local process that has it
  can read and send all your mail.
- **Your private keys** are unlocked in peroxide's memory for as long as
  it runs, after the first client login. Anyone who can read that
  process's memory (root, a debugger, a core dump) can take them, and
  with them your login and mailbox passwords and the bridge password,
  which peroxide keeps there to renew the session (`TODO.md` section
  22).

## What is on disk

| | Content | Protection |
|---|---|---|
| `auth.json` | Session tokens, **login password**, **mailbox password**, key salts | NaCl secretbox under a key that *is* the bridge password; file mode 0600, directory 0700 |
| `<user>.db` | Message metadata of every message listed: subjects, addresses, dates, labels, attachment names | none, file mode 0700, directory 0700 |
| message store, `~/.local/share/peroxide/<user>/` | Every message as served to clients, decrypted, one file each; in `<user>.db`, its structure and envelope | AES-256-GCM under a key derived from the bridge password, never stored; files 0600, directory 0700 |
| `-debug` output, if you keep it | Decrypted mail and metadata | whatever you give it |

Unlocked keys and the bridge password are never written by peroxide;
decrypted mail only sealed in the message store (below), or by
`-debug`. See [accounts.md](accounts.md#files-on-disk).

Consequences:

- **The bridge password is as sensitive as your Proton password.** With
  `auth.json` it gives your Proton login and mailbox passwords, and
  with the message store your mail. How to keep it:
  [accounts.md](accounts.md#what-the-stored-credentials-mean).
- **Full-disk encryption matters.** `<user>.db` is the metadata of your
  mailbox, unencrypted. Proton doesn't encrypt subjects and addresses
  end to end either (it needs them to list and route mail), but on your
  disk peroxide adds no protection of its own.
- `peroxide auth -remove` leaves `<user>.db`, the message store and the
  Proton session behind
  ([accounts.md](accounts.md#files-on-disk)).

## The message store

peroxide keeps each message as it serves it, so that it is downloaded
and decrypted once (see [imap/messages.md](imap/messages.md)). The
files are under `$XDG_DATA_HOME/peroxide/<user>/` (else
`~/.local/share/peroxide/<user>/`): mail is data, so it is where backups
look, not in a cache directory.

- **The key** is HKDF-SHA256 of the 32 random bytes the bridge password
  encodes (the key sealing `auth.json`), salted with your Proton user
  ID and labelled `peroxide message store v1`: a different key from
  `auth.json`'s, and one per account. It exists in memory while
  peroxide runs, and is never written or logged. No slow KDF is needed,
  as its input is random, not a password a person chose.
- **Each file** is sealed with AES-256-GCM, a random nonce per write,
  and the message ID as additional data, so a file renamed to another
  message's name fails to open rather than serving the wrong mail. The
  record of each message (its BODYSTRUCTURE, envelope, size) is sealed
  the same way in `<user>.db`. File names are the hex of Proton's
  message IDs, which the metadata database holds in clear anyway.
- **A new bridge password** (`peroxide auth` again) or a lost one makes
  the next login find a key that doesn't match (`<user>.db` keeps an
  HMAC check value of it, not the key): the database and the store are
  wiped and downloaded again, as after a Refresh. The files sealed
  under the old key stay readable with the old password to whoever
  copied them before; a new bridge password protects what is stored
  after it.
- **What it protects**: your mail at rest is as safe as the bridge
  password, even without full-disk encryption. It doesn't hide how
  many messages you have, their sizes, or, through `<user>.db`, their
  metadata.

## Network exposure

- Default: IMAP and SMTP on `127.0.0.1`, implicit TLS with peroxide's
  own certificate, which is no CA (see
  [commands.md](commands.md#listening-and-tls)). A client that checks
  it can't be fooled by another local user listening on 1143 or 1025
  while `serve` is down; one with checking off (aerc's `+insecure`)
  can, and gives that impostor the bridge password.
- `serve -plaintext`: no TLS, loopback IP addresses only, passwords
  accepted in clear: open to that impostor, and to anything that can
  read loopback traffic.
- `-imap-host`/`-smtp-host` other than loopback: still TLS (never
  plain text), ideally with `-tls-client-ca`.
- Human verification opens no port: `peroxide auth` sends you to
  Proton's verification page, whose URL carries the single-use
  challenge token of that login attempt.

## Crypto peroxide does and doesn't do

Encryption and signatures of messages, sent and received, are in
[encrypt.md](encrypt.md#crypto-peroxide-does-and-doesnt-do). This page
is about protecting peroxide itself: your secrets, its files, its ports.

## Logs

Without `-debug`, peroxide logs operations: logins, mailbox syncs,
folder and address changes, the file names of attachments it uploads
when sending, errors. Lines may name message IDs, folder and file names
and your addresses; never message content, passwords or tokens.

With `-debug`, the log holds your mail in clear. Read
[debug.md](debug.md) before turning it on.

## How proton-bridge does it

proton-bridge keeps its secrets in `vault.enc`, encrypted with
AES-256-GCM under a key in the system keychain, and stores no login or
mailbox password: a derived key passphrase and the refresh token. Its
message store on disk (Gluon) is encrypted under a per-account random
key kept in the vault (`GluonKey`), lz4-compressed. It encrypts to
external PGP recipients as their contact settings ask. In exchange it
needs a working keychain.

peroxide derives its store key from the bridge password instead, so it
needs no keychain and keeps no key at rest. Its exposure is larger in
two places, the stored passwords and the unencrypted metadata
database. Both are fixable without a keychain (store the derived
passphrase, drop the login password, seal the metadata under the store
key); neither is done yet.
