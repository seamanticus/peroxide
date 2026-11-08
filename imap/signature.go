package imap

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/seamanticus/peroxide/protonmail"
)

// Signatures inside an encrypted body (all Proton-to-Proton mail, and mail
// Proton encrypted on arrival that was signed inline) cover the decrypted
// text, which peroxide serves in the clear: the client receives no
// signature to check. So peroxide checks it while decrypting, with the
// sender's public keys from Proton (GET /keys), and writes the verdict in
// the header SignatureHeader, as an mail server writes Authentication-Results
// (RFC 8601). docs/imap/messages.md describes the field for clients that want
// to show it. A body that isn't signed gets no field.

// SignatureHeader is the header field peroxide writes the verdict of a
// body's signature in.
const SignatureHeader = "X-Peroxide-Signature"

// The verdicts, as RFC 8601's for DKIM (section 2.7.1). "none" is not
// written, the field is left out; "temperror" isn't either: a message whose
// sender's keys can't be fetched for now isn't built (the error is
// retryable), as the verdict is part of what is stored and must not change
// under a UID.
const (
	sigPass    = "pass"      // the signature verifies, by a key of the sender
	sigFail    = "fail"      // it doesn't verify: the text was changed, or the key is wrong
	sigNeutral = "neutral"   // no key of the sender Proton knows made it
	sigPermErr = "permerror" // the sender has no key Proton knows, or Proton refused to say
)

// senderKeysTTL is how long the public keys of an address are kept: Proton
// keys rarely change, and a rotated key is seen within this.
const senderKeysTTL = 24 * time.Hour

// senderKeys caches the public keys Proton publishes for each address, per
// account: one request per sender of signed mail, then none for a day. Safe
// for concurrent use; the zero value is ready.
type senderKeys struct {
	mu      sync.Mutex
	entries map[string]senderKeysEntry
}

type senderKeysEntry struct {
	keys    openpgp.EntityList
	fetched time.Time
}

// get returns the public keys of address, asking Proton if they aren't
// cached or are older than senderKeysTTL. An address Proton publishes no key
// for (outside Proton, no WKD) has none, cached as well.
func (s *senderKeys) get(ctx context.Context, c *protonmail.Client, address string) (openpgp.EntityList, error) {
	key := strings.ToLower(address)
	s.mu.Lock()
	e, ok := s.entries[key]
	s.mu.Unlock()
	if ok && time.Since(e.fetched) < senderKeysTTL {
		return e.keys, nil
	}

	resp, err := c.GetPublicKeys(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("cannot get the public keys of %v: %w", address, err)
	}
	var keys openpgp.EntityList
	for _, pub := range resp.Keys {
		// Proton clears Trusted on a key the owner marked compromised:
		// signatures it made no longer vouch for the sender, as in
		// Proton's own clients. Left out: a body it signed is neutral beside
		// the sender's other keys, permerror if it was the only one.
		if pub.Flags&protonmail.PublicKeyTrusted == 0 {
			continue
		}
		ent, err := pub.Entity()
		if err != nil {
			log.Printf("cannot read a public key of %v: %v", address, err)
			continue
		}
		keys = append(keys, ent)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]senderKeysEntry)
	}
	s.entries[key] = senderKeysEntry{keys: keys, fetched: time.Now()}
	return keys, nil
}

// sigVerdict is the result of checking a body's signature.
type sigVerdict struct {
	result      string // one of the sig* verdicts, "" if not signed
	fingerprint []byte // of the key that made it, if known
	reason      string // for fail, neutral and the errors
}

// headerValue formats v as the value of SignatureHeader, in the shape of an
// RFC 8601 resinfo: the verdict, then a reason, the signer, the key and, on
// the message's field, the summary of its attachments' verdicts (""
// leaves it out).
func (v sigVerdict) headerValue(signer, attachments string) string {
	var b strings.Builder
	b.WriteString(v.result)
	if v.reason != "" {
		fmt.Fprintf(&b, " (%s)", v.reason)
	}
	if signer != "" {
		fmt.Fprintf(&b, " header.from=%s", signer)
	}
	if len(v.fingerprint) > 0 {
		fmt.Fprintf(&b, " key=%s", strings.ToUpper(hex.EncodeToString(v.fingerprint)))
	}
	if attachments != "" {
		fmt.Fprintf(&b, " attachments=%s", attachments)
	}
	return b.String()
}

// decryptAndVerify decrypts msg's body, reads it to its end and checks its
// signature, if it carries one, against the account's own keys (mail it
// sent) and the sender's public keys. It returns the plaintext, the verdict
// and the signatures it carries (for PGP/MIME, handed to the client): the
// one checked first, then those no key given matched; none made by the
// account, which the client needn't check.
// A bad signature is a verdict, not an error. The errors are a failure to
// decrypt (decryptErr) and a failure to fetch the sender's keys that may
// pass (lookupErr, retryable): the message is then not built now.
func (u *user) decryptAndVerify(ctx context.Context, msg *protonmail.Message) (body []byte, verdict sigVerdict, sigs []*packet.Signature, decryptErr, lookupErr error) {
	// Proton signs one-pass: whether the body is signed, and by which of
	// the keys given, is known before the text is read.
	md, err := msg.Read(u.privateKeys, nil)
	if err != nil {
		return nil, sigVerdict{}, nil, err, nil
	}
	senderKnown := md.SignedBy != nil // by the account: mail it sent
	if md.IsSigned && md.SignedBy == nil && msg.Sender != nil && msg.Sender.Address != "" {
		keys, err := u.senders.get(ctx, u.c, msg.Sender.Address)
		switch {
		case err != nil && (retryable(err) || ctx.Err() != nil):
			return nil, sigVerdict{}, nil, nil, err
		case err != nil:
			log.Printf("Message %v: cannot check its signature: %v", msg.ID, err)
			verdict = sigVerdict{result: sigPermErr, reason: "Proton refused the sender's keys"}
		case len(keys) > 0:
			// Decrypted again, the sender's keys added, to verify as it
			// reads.
			senderKnown = true
			keyring := append(slices.Clone(u.privateKeys), keys...)
			if md, err = msg.Read(keyring, nil); err != nil {
				return nil, sigVerdict{}, nil, err, nil
			}
		}
	}

	// A bad signature doesn't fail the read: go-crypto reports it in
	// md.SignatureError once the text is read to its end.
	body, err = io.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, sigVerdict{}, nil, err, nil
	}
	if md.IsSigned && md.Signature != nil && !signedByAccount(md, u.privateKeys) {
		sigs = append(sigs, md.Signature)
	}
	sigs = append(sigs, md.UnverifiedSignatures...)
	switch {
	case !md.IsSigned:
		return body, sigVerdict{}, nil, nil, nil
	case verdict.result != "":
		return body, verdict, sigs, nil, nil
	default:
		return body, judge(md, senderKnown), sigs, nil, nil
	}
}

// signedByAccount reports whether one of the account's own keys made md's
// signature.
func signedByAccount(md *openpgp.MessageDetails, account openpgp.EntityList) bool {
	return md.SignedBy != nil && len(account.KeysById(md.SignedByKeyId)) > 0
}

// judge turns what go-crypto found, the body read to its end, into a
// verdict. senderKnown says the keyring held keys of the sender.
func judge(md *openpgp.MessageDetails, senderKnown bool) sigVerdict {
	switch {
	case md.SignedBy == nil && !senderKnown:
		return sigVerdict{result: sigPermErr, reason: "Proton publishes no key for the sender"}
	case md.SignedBy == nil:
		return sigVerdict{result: sigNeutral, reason: "signed by a key the sender doesn't publish"}
	case md.SignatureError != nil:
		return sigVerdict{result: sigFail, fingerprint: md.SignedBy.PublicKey.Fingerprint, reason: md.SignatureError.Error()}
	default:
		return sigVerdict{result: sigPass, fingerprint: md.SignedBy.PublicKey.Fingerprint}
	}
}

// sigNone is the attachments summary of a message none of whose attachments
// is signed; an attachment not signed gets no field of its own.
const sigNone = "none"

// verifyAttachment checks the detached signature Proton keeps for att
// (Attachment.Signature: Proton's clients sign the file before encrypting
// it, go-proton-api UploadAttachment) over its decrypted bytes, with the
// account's keys and those Proton publishes for sender. The verdict's result
// is "" if att carries no signature. lookupErr is a failure to fetch the
// sender's keys that may pass: the message is then not built now.
func (u *user) verifyAttachment(ctx context.Context, att *protonmail.Attachment, data []byte, sender string) (v sigVerdict, lookupErr error) {
	sig, err := att.SignaturePacket()
	if err != nil {
		return sigVerdict{result: sigPermErr, reason: "its signature can't be read: " + err.Error()}, nil
	}
	if sig == nil {
		return sigVerdict{}, nil
	}

	keyring := slices.Clone(u.privateKeys)
	senderKnown := false
	if sender != "" {
		keys, err := u.senders.get(ctx, u.c, sender)
		switch {
		case err != nil && (retryable(err) || ctx.Err() != nil):
			return sigVerdict{}, err
		case err != nil:
			log.Printf("Attachment %v: cannot check its signature: %v", att.ID, err)
			return sigVerdict{result: sigPermErr, reason: "Proton refused the sender's keys"}, nil
		}
		senderKnown = len(keys) > 0
		keyring = append(keyring, keys...)
	}

	_, signer, err := openpgp.VerifyDetachedSignature(keyring, bytes.NewReader(data), bytes.NewReader(sig), nil)
	switch {
	case errors.Is(err, pgperrors.ErrUnknownIssuer) && !senderKnown:
		return sigVerdict{result: sigPermErr, reason: "Proton publishes no key for the sender"}, nil
	case errors.Is(err, pgperrors.ErrUnknownIssuer):
		return sigVerdict{result: sigNeutral, reason: "signed by a key the sender doesn't publish"}, nil
	case signer == nil && err != nil:
		// A key matched the issuer, and the bytes don't verify.
		return sigVerdict{result: sigFail, fingerprint: issuerFingerprint(keyring, sig), reason: err.Error()}, nil
	case err != nil:
		// Verified, but the key or the signature is expired or revoked.
		return sigVerdict{result: sigFail, fingerprint: signer.PrimaryKey.Fingerprint, reason: err.Error()}, nil
	default:
		return sigVerdict{result: sigPass, fingerprint: signer.PrimaryKey.Fingerprint}, nil
	}
}

// issuerFingerprint returns the fingerprint of the primary key of the key in
// keyring that the signature packets sig name as issuer, nil if none.
func issuerFingerprint(keyring openpgp.EntityList, sig []byte) []byte {
	p, err := packet.NewReader(bytes.NewReader(sig)).Next()
	if err != nil {
		return nil
	}
	s, ok := p.(*packet.Signature)
	if !ok || s.IssuerKeyId == nil {
		return nil
	}
	if keys := keyring.KeysById(*s.IssuerKeyId); len(keys) > 0 {
		return keys[0].Entity.PrimaryKey.Fingerprint
	}
	return nil
}

// attachmentsSummary sums up the verdicts of a message's attachments, for
// the attachments property of its field: the worst of them, fail before
// permerror before neutral before pass, an unsigned attachment counting as
// neutral beside signed ones; "none" when none is signed, "" when it has
// no attachment.
func attachmentsSummary(verdicts []sigVerdict) string {
	if len(verdicts) == 0 {
		return ""
	}
	rank := map[string]int{sigPass: 0, sigNeutral: 1, "": 1, sigPermErr: 2, sigFail: 3}
	worst, signed := sigPass, false
	for _, v := range verdicts {
		if v.result != "" {
			signed = true
		}
		r := v.result
		if r == "" {
			r = sigNeutral
		}
		if rank[r] > rank[worst] {
			worst = r
		}
	}
	if !signed {
		return sigNone
	}
	return worst
}
