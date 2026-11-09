# The IMAP server

peroxide's IMAP server is go-imap v1's server (`github.com/emersion/go-imap`
v1.2.1) with peroxide's backend behind it (package `imap`). It speaks
IMAP4rev1 (RFC 3501) and announces:

    IMAP4rev1 LITERAL+ SASL-IR CHILDREN UNSELECT MOVE IDLE APPENDLIMIT=73400320 AUTH=PLAIN

(`AUTH=PLAIN` before login; `STARTTLS` never, as TLS is implicit when
enabled, see [../commands.md](../commands.md#listening-and-tls).)

Not supported: UIDPLUS (no COPYUID/APPENDUID), CONDSTORE/QRESYNC,
NOTIFY, ID, ENABLE, SPECIAL-USE as a capability (the attributes are
sent in LIST anyway).
CREATE, DELETE and RENAME work under `Folders/` and `Labels/` only
([changes.md](changes.md#create-delete-rename)); subscriptions are kept
by peroxide ([mailboxes.md](mailboxes.md#list-lsub-subscribe)).

## Pages

- [mailboxes.md](mailboxes.md): which mailboxes exist and what Proton
  object each one is; flags; LIST and STATUS.
- [sync.md](sync.md): the local database, Proton's event stream, how
  changes made elsewhere reach your client, and UIDs.
- [messages.md](messages.md): FETCH and SEARCH, how a message is built
  from Proton's data and stored, decryption, mail peroxide can't
  decrypt, and messages that change.
- [changes.md](changes.md): STORE, COPY, MOVE, EXPUNGE and APPEND, and
  what each costs.

## One account, many clients

Every connection logged in as the same user shares one account object
(`imap/user.go`): one database, one event receiver, one set of
mailboxes. A change one client makes is sent to the others that have
the mailbox selected (EXISTS, EXPUNGE, FETCH FLAGS). The account closes
when its last connection logs out.

Several accounts can be logged in at once, each with its own database
and its own poll of Proton's events.

## Limits worth knowing

- **APPEND size.** 70 MiB (73400320 bytes), the limit of Proton's
  import API on an encrypted message ([changes.md](changes.md#append)).
  A larger literal is refused before it is read: the
  client gets `BAD`, as go-imap reads literals before the command sees
  them, and has no way to answer `NO [TOOBIG]` then.
- **Memory.** go-imap reads every literal whole into memory. The cap
  above is also the cap on that.
- **Updates.** go-imap delivers unsolicited updates in no fixed order,
  and at any time, also during a FETCH. `TODO.md` section 11, items 6
  and 7.
- **Debug traces.** go-imap reads the continuation lines of
  `AUTHENTICATE` and the `DONE` ending `IDLE` past its debug writer, so
  `-debug` logs never show them (see [../debug.md](../debug.md)).

## How proton-bridge does it

proton-bridge uses Gluon, Proton's own IMAP server library, which keeps
a full local copy of every message in an encrypted store, assigns and
persists UIDs, and supports UIDPLUS, MOVE, IDLE, UNSELECT, ID and more.
peroxide's backend keeps the same kind of store and persistent UIDs,
and is far smaller, at the cost of the gaps listed above.
