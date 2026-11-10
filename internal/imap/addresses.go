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
// Proton to the address list, which drafts take their sender from.
//
// Private keys are unlocked once per peroxide process, at its first login
// for the account (auth.Manager caches the session): a key the address didn't
// have then can't be used until peroxide restarts. It is reported, not
// unlocked.
func (u *user) handleAddressEvent(event *protonmail.EventAddress) {
	switch event.Action {
	case protonmail.EventCreate, protonmail.EventUpdate, protonmail.EventUpdateFlags:
		if event.Address == nil {
			log.Printf("cannot handle event for address %v: no address", event.ID)
			return
		}
		u.putAddress(event.Address)
		u.reportLockedKeys(event.Address)
	case protonmail.EventDelete:
		u.deleteAddress(event.ID)
	default:
		// Not a broken invariant: the action comes from the API.
		log.Printf("cannot handle event for address %v: unknown action %v", event.ID, event.Action)
	}
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

// reportLockedKeys logs the active keys of addr that weren't unlocked: mail
// encrypted to them can't be read, nor drafts signed with them made, until
// peroxide restarts.
func (u *user) reportLockedKeys(addr *protonmail.Address) {
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
		if !slices.ContainsFunc(unlocked, func(e *openpgp.Entity) bool {
			return e.PrimaryKey.KeyId == entity.PrimaryKey.KeyId
		}) {
			log.Printf("Key %v of address <%v> is new: it is unlocked when peroxide restarts", key.Fingerprint, addr.Email)
		}
	}
}
