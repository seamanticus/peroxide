# Overview

peroxide is one program that runs on your machine and translates
between standard mail protocols and Proton's API:

    mail client --IMAP--> peroxide --HTTPS--> Proton API
    mail client --SMTP--> peroxide --HTTPS--> Proton API

Proton stores mail end-to-end encrypted with OpenPGP keys that only
your password can unlock. A normal mail client can't talk to that. The
bridge logs in to Proton for you, unlocks your keys in memory, decrypts
mail as the client fetches it, and encrypts mail as the client sends
it. The client only ever talks to peroxide, on your own machine, using
a separate password peroxide made up (the *bridge password*, see
[accounts.md](accounts.md)).

peroxide started as a fork of
[hydroxide](https://codeberg.org/emersion/hydroxide). It keeps only the
IMAP and SMTP parts: no CardDAV, CalDAV or contacts.

## The parts

| Package | Role |
|---|---|
| `cmd/peroxide` | Command line, server start-up, debug logging (`debuglog.go`) |
| `auth` | The stored credentials (`auth.json`) and the in-memory sessions |
| `captcha` | The local helper page for Proton's human verification |
| `protonmail` | The Proton API client: requests, crypto helpers, log redaction |
| `events` | Polls Proton's event stream and hands pages to each account |
| `imap` | The IMAP backend: mailboxes, FETCH, STORE, COPY, MOVE, EXPUNGE |
| `imap/database` | The local bolt database of message metadata, per account |
| `smtp` | The SMTP backend: turns a submitted message into a Proton send |
| `config` | File locations and the TLS configuration |

The IMAP and SMTP protocol work (parsing commands, writing responses) is
done by Simon Ser's libraries, `github.com/emersion/go-imap` (v1) and
`github.com/emersion/go-smtp`. peroxide implements their backend
interfaces. Some of what peroxide can and can't do comes from them, and
the pages say so where it matters.

## What happens when

**`peroxide auth <user>`** logs in to Proton once, interactively,
unlocks your keys to check the password, invents a bridge password,
encrypts your credentials with it, writes them to `auth.json`, and
prints the bridge password. Nothing runs in the background.

**`peroxide serve`** starts an SMTP server (default `127.0.0.1:1025`)
and an IMAP server (default `127.0.0.1:1143`). It does nothing until a
client logs in.

**A client logs in** with your Proton username and the bridge password.
peroxide decrypts the matching `auth.json` entry with it, refreshes the
Proton session, and unlocks your private keys. The unlocked keys stay
in memory for as long as the process runs: a second login, from any
client, only checks the password against a bcrypt hash and reuses them.

**The IMAP account opens.** peroxide lists your Proton folders and
labels, opens `~/.config/peroxide/<user>.db`, and starts polling
Proton's events every 20 to 40 seconds. The first time, it also starts
downloading every message's metadata into the database, newest first,
in the background; mailboxes fill as it goes, and later logins only
apply events. See [imap/sync.md](imap/sync.md).

**The client fetches a message.** Flags, envelope and size come from
the local database. Anything that needs the body (BODYSTRUCTURE, a body
section) downloads the full message from Proton, decrypts it, and
rebuilds a MIME message from Proton's parts. See
[imap/messages.md](imap/messages.md).

**The client changes something** (marks read, moves, deletes): peroxide
makes the matching Proton API call, applies the result to the local
database at once, tells the client, and asks Proton for events soon
after to pick up any side effects. See [imap/changes.md](imap/changes.md).

**The client sends a message over SMTP.** peroxide parses it, creates a
draft in Proton, uploads the attachments encrypted, looks up each
recipient's public key, and asks Proton to send. Proton files the sent
copy in Sent. See [smtp.md](smtp.md).

**The last IMAP client logs out.** Polling stops, the database closes.
The keys stay unlocked in memory until the process exits.

## What peroxide keeps where

- On disk, in `~/.config/peroxide/` (or `$XDG_CONFIG_HOME/peroxide/`):
  `auth.json` (credentials, encrypted) and one `<user>.db` per account
  (message metadata, not bodies). See [accounts.md](accounts.md).
- In memory only: the unlocked private keys, the Proton session, and up
  to 32 full messages per account (encrypted, as Proton sent them).
- Nowhere: decrypted message bodies, the bridge password.

## How proton-bridge does it

proton-bridge is a desktop application with a GUI and a CLI. It keeps
a full encrypted copy of your mailbox on disk (its Gluon store), syncs
everything in the background after login, and keeps its credentials in
an encrypted vault whose key is in your system keychain. It adds
telemetry, crash reports, automatic updates and a GUI.

peroxide is the opposite trade-off: one static binary, no keychain, no
GUI, no full local copy. Bodies are fetched on demand. That keeps it
small (memory and disk) and simple to audit, and costs requests: a
folder opened for the first time in a session lists all its messages
again, and every body is downloaded when first read. `TODO.md` sections
8, 9 and 9.1 track the parts of that cost peroxide intends to remove.
