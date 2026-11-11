package imap

import (
	"log"
	"slices"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// addresses returns the addresses of the account. The slice is never changed
// in place: handleAddressEvent replaces it.
func (u *user) addresses() []*protonmail.Address {
	u.Lock()
	defer u.Unlock()

	return u.addrs
}

// handleAddressEvent applies an address created, updated or deleted in
// Proton to the address list, which drafts take their sender from. It
// reports whether the address has a key the session hasn't unlocked, seen
// for the first time: the caller then has the clients log in again
// (user.relogin).
func (u *user) handleAddressEvent(event *protonmail.EventAddress) (newKey bool) {
	switch event.Action {
	case protonmail.EventCreate, protonmail.EventUpdate, protonmail.EventUpdateFlags:
		if event.Address == nil {
			log.Printf("cannot handle event for address %v: no address", event.ID)
			return false
		}
		u.putAddress(event.Address)
		return u.markLockedKeys(event.Address)
	case protonmail.EventDelete:
		u.deleteAddress(event.ID)
	default:
		// Not a broken invariant: the action comes from the API.
		log.Printf("cannot handle event for address %v: unknown action %v", event.ID, event.Action)
	}
	return false
}

func (u *user) putAddress(addr *protonmail.Address) {
	u.Lock()
	defer u.Unlock()

	addrs := slices.Clone(u.addrs)
	if i := slices.IndexFunc(addrs, func(a *protonmail.Address) bool { return a.ID == addr.ID }); i >= 0 {
		addrs[i] = addr
	} else {
		log.Printf("Address %v <%v> created", addr.ID, addr.Email)
		addrs = append(addrs, addr)
	}
	u.addrs = addrs
}

func (u *user) deleteAddress(addressID string) {
	u.Lock()
	defer u.Unlock()

	u.addrs = slices.DeleteFunc(slices.Clone(u.addrs), func(a *protonmail.Address) bool {
		if a.ID == addressID {
			log.Printf("Address %v <%v> deleted", a.ID, a.Email)
			return true
		}
		return false
	})
}

// markLockedKeys marks the session stale (auth.Manager.MarkStale) for each
// active key of addr that isn't unlocked: mail encrypted to it can't be
// read, nor drafts signed with it made, until the account logs in again. It
// reports whether a key was marked for the first time, so that a key which
// stays locked doesn't have the clients log in again at every event.
func (u *user) markLockedKeys(addr *protonmail.Address) (newKey bool) {
	u.Lock()
	unlocked := u.privateKeys
	u.Unlock()

	for _, key := range addr.Keys {
		if key.Active != 1 {
			continue
		}
		entity, err := key.Entity()
		if err != nil {
			log.Printf("cannot read key %v of address <%v>: %v", key.ID, addr.Email, err)
			continue
		}
		if slices.ContainsFunc(unlocked, func(e *openpgp.Entity) bool {
			return e.PrimaryKey.KeyId == entity.PrimaryKey.KeyId
		}) {
			continue
		}
		if u.backend.sessions.MarkStale(u.username, key.ID) {
			log.Printf("Key %v of address <%v> is new: it is unlocked at the next login", key.Fingerprint, addr.Email)
			newKey = true
		} else {
			log.Printf("Key %v of address <%v> isn't unlocked", key.Fingerprint, addr.Email)
		}
	}
	return newKey
}
