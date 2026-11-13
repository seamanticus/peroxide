# peroxide manual

This manual says what peroxide does, how it does it, and what that means
for you. It is written for people who build peroxide from source and
run it themselves. It assumes you know what IMAP, SMTP and OpenPGP are,
but not how Proton's API or peroxide's code work.

Each page covers one subject, and each fact has one home: other pages
link to it instead of repeating it.

The commit history is the record of changes. This manual describes the
code as it is at the commit you are reading it from.

## Contents

- [Overview](overview.md): what peroxide is, its parts, and how a
  request travels through them.
- [Commands and options](commands.md): every command and flag.
- [Accounts](accounts.md): logging in, the bridge password, what is
  stored on disk, keys, sessions and human verification (CAPTCHA).
- [The IMAP server](imap/README.md): what it supports, and its limits.
  - [Mailboxes](imap/mailboxes.md): folders, labels, system mailboxes,
    LIST and STATUS.
  - [Sync](imap/sync.md): the local database, Proton's events, polling
    and UIDs.
  - [Messages](imap/messages.md): FETCH, how messages are rebuilt from
    Proton's data, and what happens to mail peroxide can't decrypt.
  - [Changing mail](imap/changes.md): STORE, COPY, MOVE, EXPUNGE and
    APPEND.
- [SMTP](smtp.md): sending mail, and `peroxide sendmail`.
- [Encryption](encrypt.md): signing and encrypting mail in your client,
  what each choice gets you and your recipients, to Proton users and
  others, and what you get when others do.
- [Debugging](debug.md): the `-debug` flag, what it hides, what it
  doesn't, and how to treat its output.
- [Security](security.md): what peroxide protects, what it trusts, and
  what is exposed.

## Conventions

- `code` is a name in the code or a literal value. File paths are
  relative to the repository root unless they start with `~`.
- "Proton" is the service and its API; "the client" is your mail
  program (aerc, Thunderbird, mutt...).
- RFC numbers refer to the IETF documents: RFC 3501 is IMAP4rev1,
  RFC 5321 is SMTP, RFC 3156 is PGP/MIME.
- Pages are listed above in reading order; the footer of each page
  links the previous one, these contents and the next one.

<!-- nav -->

---

[Next: Overview](overview.md)
