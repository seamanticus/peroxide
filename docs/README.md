# peroxide manual

This manual says what peroxide does, how it does it, and what that means
for you. It is written for people who build peroxide from source and
run it themselves. It assumes you know what IMAP, SMTP and OpenPGP are,
but not how Proton's API or peroxide's code work.

Each page covers one subject. Where Proton's own bridge
([proton-bridge](https://github.com/ProtonMail/proton-bridge)) behaves
differently, a section headed "How proton-bridge does it" says how, and
why peroxide does or doesn't follow it. Open problems are tracked in
`TODO.md` at the root of the repository; pages point to its sections
instead of repeating them.

The commit history is the record of changes. This manual describes the
code as it is at the commit you are reading it from.

## Contents

- [overview.md](overview.md): what peroxide is, its parts, and how a
  request travels through them.
- [commands.md](commands.md): every command and flag.
- [accounts.md](accounts.md): logging in, the bridge password, what is
  stored on disk, keys, sessions and human verification (CAPTCHA).
- [imap/](imap/README.md): the IMAP server.
  - [imap/mailboxes.md](imap/mailboxes.md): folders, labels, system
    mailboxes, LIST and STATUS.
  - [imap/sync.md](imap/sync.md): the local database, Proton's events,
    polling and UIDs.
  - [imap/messages.md](imap/messages.md): FETCH, how messages are
    rebuilt from Proton's data, and what happens to mail peroxide can't
    decrypt.
  - [imap/changes.md](imap/changes.md): STORE, COPY, MOVE, EXPUNGE and
    APPEND.
- [smtp.md](smtp.md): sending mail, and `peroxide sendmail`.
- [encrypt.md](encrypt.md): signing and encrypting mail in your client,
  what each choice gets you and your recipients, to Proton users and
  others, and what you get when others do.
- [debug.md](debug.md): the `-debug` flag, what it hides, what it
  doesn't, and how to treat its output.
- [security.md](security.md): what peroxide protects, what it trusts,
  and what is exposed.

## Conventions

- `code` is a name in the code or a literal value. File paths are
  relative to the repository root unless they start with `~`.
- "Proton" is the service and its API; "the client" is your mail
  program (aerc, Thunderbird, mutt...).
- RFC numbers refer to the IETF documents: RFC 3501 is IMAP4rev1,
  RFC 5321 is SMTP, RFC 3156 is PGP/MIME.
