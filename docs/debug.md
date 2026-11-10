# The `-debug` flag

    peroxide -debug serve > peroxide.log

`-debug` makes peroxide log, as they happen and in one stream:

- every Proton API request and response,
- every IMAP and SMTP session, line by line, in both directions,
- what peroxide logs anyway (logins, syncs, events, errors), prompts,
  the IMAP and SMTP servers' own error logs, and a crash's stack trace.

It is meant for finding out why something misbehaves: what a client
sent, what peroxide asked Proton, what Proton answered, and in which
order. **The log contains your mail.** Read the rest of this page
before you turn it on or share a log.

## Where it goes

Without `-debug`, peroxide logs to stderr, and logs only its own
messages.

With `-debug`, everything goes to **stdout**, interleaved in order:
`peroxide -debug serve > file` captures the whole session, as `2>&1`
would. The runtime's crash output (written to file descriptor 2) is
copied there too (`runtime/debug.SetCrashOutput`).

Each IMAP or SMTP connection's lines are written whole: two clients'
lines never mix within a line, and an API request logged while a
client line is half received lands after it, not inside it.

## What a log looks like

    >> GET /messages/kPAWhk...
    << GET /messages/kPAWhk... 200 OK
    {"Code":1000,"Message":{"ID":"kPAWhk...","Subject":"Hello",...}}
    a3 UID FETCH 1:* (UID FLAGS BODYSTRUCTURE)
    * 1 FETCH (UID 1 FLAGS (\Seen) BODYSTRUCTURE (...))
    a3 OK UID FETCH completed

- `>> METHOD path?query`: a request, with its query string.
- the request's JSON body, redacted (below), on the next line, when it
  has one.
- `<< METHOD path status`: the response.
- the response body as Proton sent it, redacted, on the next line. A
  response that isn't JSON is not logged; a line says how many bytes of
  which type were withheld.
- An attachment download logs only `<< GET /attachments/<id> 200 OK
  (n bytes)`: its encrypted data is never logged.
- IMAP and SMTP lines are logged as sent, `\r\n` included.

Responses are logged as received, before peroxide decodes them, so
fields peroxide doesn't know about are visible too: that is the point
of the log.

**`<tag> NO Expected DONE` after `+ idling`** means the client closed the
connection while in IDLE, without the `DONE` and `LOGOUT` RFC 2177 and
3501 expect: aerc does so when it quits (aerc_implementation.md section
7). go-imap's IDLE reads one more line, finds the connection closed and
answers NO, to a socket nobody reads any more: no client ever receives
it, and nothing is lost; the next line is the logout. A NO after
`+ idling` on a connection that stays open would be a real error.

## What is hidden

**Credentials and secrets in API traffic.** Any JSON object key, at any
depth, named (case-insensitive) one of:

    AccessToken RefreshToken Token HumanVerificationToken
    ClientEphemeral ClientProof ServerEphemeral ServerProof
    Salt KeySalt PrivateKey Password LoginPassword MailboxPassword
    TwoFactorCode Cookie RecoverySecret Key

has its value replaced by `<credentials removed>`. That covers
the SRP exchange (proofs, ephemerals, salts), the session tokens, the
TOTP code, the encrypted private keys and key salts (`/keys/salts`,
`/users`, `/addresses`), the recovery secret, and the cleartext session
keys of messages sent to recipients without a key (`Key` in a send
package). A body that isn't valid JSON is replaced entirely.

**Headers** are never logged, so `Authorization: Bearer ...`,
`x-pm-uid` and cookies don't appear.

**IMAP logins.** `LOGIN` keeps only the command: `a1 LOGIN <API key
removed for security>`, username included. When the credentials come
as literals, every client line after it is hidden until the server
answers that tag. `AUTHENTICATE` keeps the mechanism (`PLAIN`), hides
the initial response, and hides every client line after it until the
server answers its tag, pipelined commands included. go-imap reads
AUTHENTICATE's continuation lines (the SASL response) without passing
them through the log, so they never appear anyway.

**SMTP logins.** `AUTH PLAIN <...>` keeps the mechanism; every client
line after it is hidden until the server's final answer to the
exchange.

**Prompts.** Password prompts write only their label; the password
isn't echoed, and isn't logged. The TOTP code is echoed by your
terminal as you type it, but isn't logged.

## What is NOT hidden

Treat a `-debug` log as a copy of your mailbox.

- **Message content, in clear.** IMAP FETCH responses carry the
  decrypted messages your client reads: bodies, attachments (base64),
  headers. SMTP DATA carries every message you send, in clear, with its
  attachments. APPEND carries every message your client saves, drafts
  and copies.
- **Message metadata from the API.** Subjects, sender and recipient
  names and addresses, dates, sizes, labels, folder names, attachment
  file names: everything a listing shows.
- **Encrypted bodies** as Proton returns them (`GET /messages/<id>`):
  armored PGP messages, safe without your keys, which the log doesn't
  hold.
- **Query strings and request bodies.** Request lines include the
  query: the address of every public-key lookup (`/keys?Email=...`) and
  list filters. The download's requests (`POST /mail/v4/messages`) carry
  message IDs in their JSON body. Message, label and address IDs are in
  paths.
- **Your username and addresses**, in API responses and in the SMTP
  `MAIL FROM`/`RCPT TO` lines.
- **Session identifiers that aren't credentials**: the session UID in
  responses (`UID` field of `/auth`), event IDs.
- **What the deny list misses.** Redaction is by key name. A field
  Proton adds tomorrow under a new name (a new kind of token) is logged
  until its name is added to `redactedJSONFields`
  (internal/protonmail/redact.go). The tests check the known ones.

## How to treat a log

- Write it to a file only you can read (`umask 077` first), not to a
  shared directory, and delete it when done.
- Before sharing one (a bug report), cut it to the lines that matter
  and read every line you keep. Replace subjects, addresses and
  message content. Search it for your password and bridge password to
  be sure (`grep -c`); they should never be there.
- Don't run `-debug` permanently. It writes every message you read and
  send to disk in clear, which defeats the point of end-to-end
  encryption at rest. Under systemd, stdout goes to the journal: a
  `-debug` service puts your mail in the journal.
- A log taken with an old peroxide may hide less: check
  `git log -- internal/protonmail/redact.go cmd/peroxide/debuglog.go`.

## How proton-bridge does it

proton-bridge has `--log-level debug`, which logs its own operations to
size-capped, pruned log files in its data directory, and separate
`--log-imap (client|server|all)` and `--log-smtp` flags for protocol
traces, which its help describes as "may contain decrypted data!".

peroxide puts everything in one stream instead, because the useful
question is usually about order: which client command led to which API
call, and which event reached which client. One flag, one file.
