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
  process's memory (root, a debugger, a core dump) can take them.

## What is on disk

| | Content | Protection |
|---|---|---|
| `auth.json` | Session tokens, **login password**, **mailbox password**, key salts | NaCl secretbox under a key that *is* the bridge password |
| `<user>.db` | Message metadata of every message listed: subjects, addresses, dates, labels, attachment names | none, file mode 0700, directory 0700 |
| `-debug` output, if you keep it | Decrypted mail and metadata | whatever you give it |

Decrypted bodies, unlocked keys and the bridge password are never
written by peroxide (except by `export-secret-keys`, to its stdout, and
by `-debug`). See [accounts.md](accounts.md#files-on-disk).

Consequences:

- **The bridge password is as sensitive as your Proton password.** With
  `auth.json` it gives your Proton login and mailbox passwords. Keep it
  in a password manager or a gpg-encrypted file the client decrypts on
  demand, not in a client's configuration in plain text.
- **Full-disk encryption matters.** `<user>.db` is the metadata of your
  mailbox, unencrypted. Proton doesn't encrypt subjects and addresses
  end to end either (it needs them to list and route mail), but on your
  disk peroxide adds no protection of its own.
- `peroxide auth -remove` deletes the credentials, not `<user>.db`, and
  doesn't end the session on Proton's side.

## Network exposure

- Default: IMAP and SMTP on `127.0.0.1`, plain text, passwords
  accepted over it. Safe from the network, not from other local users
  or processes (who still need the bridge password).
- `-imap-host`/`-smtp-host` other than loopback: use `-tls-cert`
  and `-tls-key`, ideally `-tls-client-ca`. Without TLS, the bridge
  password and every message cross the network in clear.
- The CAPTCHA helper listens on `127.0.0.1:8765` only while
  `peroxide auth` waits for a challenge, behind a random single-use
  URL, and warns if you make it listen elsewhere.

## Crypto peroxide does and doesn't do

- Decrypts with your unlocked keys (ProtonMail's go-crypto, an OpenPGP
  implementation); serves mail it can't decrypt as armored text or
  PGP/MIME, never as a guess (see [imap/messages.md](imap/messages.md)).
- **Doesn't verify signatures** of received mail, so a forged sender
  isn't flagged.
- Sends Proton-to-Proton mail encrypted and signed; sends mail to other
  addresses unencrypted and unsigned (see [smtp.md](smtp.md)). Proton
  applies its own encryption at rest either way.
- Doesn't check that a recipient's key is the right one: it trusts
  Proton's key server.

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
message store on disk (Gluon) is encrypted under a per-account key kept
in the vault (`GluonKey`). It encrypts to external PGP recipients as
their contact settings ask. In exchange it needs a working keychain and
keeps a full copy of your mail on disk.

peroxide's exposure is larger in two places, the stored passwords and
the unencrypted metadata database, and smaller in one: no copy of your
messages on disk at all. Both of the former are fixable without a
keychain (store the derived passphrase, drop the login password,
encrypt the database under the bridge password's key); none is done
yet.
