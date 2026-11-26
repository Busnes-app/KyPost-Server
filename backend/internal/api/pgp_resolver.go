package api

import (
	"context"
	"net/mail"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

// resolveTier identifies which rung of the discovery ladder produced a
// resolvedKey, so callers (send-time encryption, UI) can decide whether to
// use the key silently, prompt for confirmation, or send unencrypted.
type resolveTier string

const (
	// tierContactVerified is a usable key already pinned to a local contact
	// — used silently, no discovery performed.
	tierContactVerified resolveTier = "verified"
	// tierWKD is a key freshly discovered via Web Key Directory — auto-
	// trusted (the sender's own domain vouches for it) and pinned when
	// settings.StoreDiscoveredKeys is set.
	tierWKD resolveTier = "wkd"
	// tierKeyserverConfirm is a key found on a public keyserver — these are
	// unauthenticated by anyone the sender already trusts, so the caller
	// must get explicit user confirmation before it is pinned or used.
	tierKeyserverConfirm resolveTier = "keyserver_confirm"
	// tierKeyChanged means discovery found a key for this address whose
	// fingerprint differs from the one already pinned to the contact — a
	// TOFU (trust-on-first-use) violation. The resolver refuses to switch;
	// the caller must not encrypt to the newly discovered key.
	tierKeyChanged resolveTier = "key_changed"
	// tierNone means no usable key could be resolved by any means.
	tierNone resolveTier = "none"
)

// resolvedKey is the outcome of running the discovery ladder for one
// recipient address.
type resolvedKey struct {
	Armored     string
	Fingerprint string
	Tier        resolveTier
	Usable      bool
	// Err is set when the address book could not be read at all. It is not
	// tierNone: tierNone means "this recipient has no key", which the send
	// path answers by offering the plaintext pickup fallback. A storage fault
	// reported as tierNone would invite the sender to downgrade a recipient
	// whose pinned key was there all along.
	Err error
	// Capability is the best key class the recipient offers ("pq", "v6",
	// "v4"; "" with no key). Additive and informational in P0: Armored is
	// still the v4 key whenever there is one, because no KyPost sender can
	// encrypt to a v6 PQC key yet (openpgp.js 6.3.3). P4 switches.
	Capability string
}

// keyResolver runs the PGP key discovery ladder — pinned contact key → WKD
// → keyserver — for one user's outgoing mail, honoring that user's
// pgpdiscovery.Settings and TOFU pinning against their contacts store.
type keyResolver struct {
	store    *contacts.Store
	settings pgpdiscovery.Settings
	// discover gates whether WKD/keyserver lookups run at all; false means
	// only the already-pinned contact key is considered (e.g. discovery
	// disabled by policy, or a caller that only wants the local view).
	discover bool
	// suppressed is the set of normalized addresses the user has opted out of
	// discovery. A suppressed address does no WKD/keyserver lookup, pin, or
	// auto-create — it falls through to the pickup path. A key the user
	// already holds (resolve step 1) is unaffected.
	suppressed map[string]bool
}

// findContact returns the first contact whose email matches, case-
// insensitively. Unlike findKeyedContact (server_mail_send.go), this returns the
// whole Contact so callers can inspect/update provenance fields beyond
// PGPKey.
func findContact(store *contacts.Store, email string) (contacts.Contact, bool, error) {
	all, err := store.List()
	if err != nil {
		return contacts.Contact{}, false, err
	}
	target := strings.ToLower(strings.TrimSpace(email))
	for _, c := range all {
		for _, e := range c.Emails {
			if strings.ToLower(strings.TrimSpace(e.Value)) == target {
				return c, true, nil
			}
		}
	}
	return contacts.Contact{}, false, nil
}

// pin writes discovered keys + provenance to the matching contact, creating a
// minimal contact if none exists yet. Upsert assigns a UID when c.UID is
// empty. Marks each key unverified — WKD/keyserver discovery is TOFU, not
// eyeball verification — unless its fingerprint matches the one already pinned
// in its slot, in which case this is just a refresh of the same key and any
// existing source/verified provenance (e.g. a manual, eyeball-verified pin) is
// preserved rather than downgraded. v4 and v6 each go to their own slot; a v6
// key alone takes the original slot only when that holds no v4 key.
func (kr *keyResolver) pin(email string, v4, v6 *wkdCert, source string) {
	c, ok, err := findContact(kr.store, email)
	if err != nil {
		// Creating a contact here would pin a discovered key to a brand-new
		// record while an existing, possibly manually verified one sits in an
		// unreadable file. Skip the pin; the send still uses the key it holds.
		return
	}
	if !ok {
		c = contacts.Contact{
			FormattedName:    email,
			Emails:           []contacts.ContactValue{{Value: email}},
			DiscoveryCreated: true,
		}
	}
	set := func(key, fp, src *string, verified *bool, cert *wkdCert) {
		same := *fp != "" && strings.EqualFold(*fp, cert.Fingerprint)
		*key, *fp = cert.Armored, cert.Fingerprint
		if !same {
			*src, *verified = source, false
		}
	}
	if v4 != nil {
		set(&c.PGPKey, &c.PGPKeyFingerprint, &c.PGPKeySource, &c.PGPKeyVerified, v4)
	}
	if v6 != nil {
		if v4 == nil && (c.PGPKey == "" || keyVersionOf(c.PGPKey) == 6) {
			set(&c.PGPKey, &c.PGPKeyFingerprint, &c.PGPKeySource, &c.PGPKeyVerified, v6)
		} else if contacts.AuthenticatedForContact(c, email) || strings.EqualFold(c.PGPKeyV6Fingerprint, v6.Fingerprint) {
			// A v6 key added beside an existing key signs for every address on
			// the contact, so only a lookup of the address the whole record is
			// about may add one (same rule as the Autocrypt harvest).
			set(&c.PGPKeyV6, &c.PGPKeyV6Fingerprint, &c.PGPKeyV6Source, &c.PGPKeyV6Verified, v6)
		}
	}
	// best-effort pin; a valid key still encrypts this send even if
	// persistence fails
	_, _ = kr.store.Upsert(c)
}

// pickWKD chooses, per OpenPGP version, the WKD certificate to use against the
// contact's pins (certs come best first). A pinned version whose certificates
// all differ from its pin is a key change; so is an answer none of whose
// chosen certificates is pinned while the contact has a pin. A v6 key added
// beside a pinned v4 key that is still served is neither.
//
// use is the certificate to encrypt to: the pinned one when the contact has a
// pin (v4 first), so a newly served key never becomes the encryption target
// beside a pinned one; otherwise v4, else v6.
func pickWKD(c contacts.Contact, certs []wkdCert) (v4, v6, use *wkdCert, changed bool) {
	pins := map[bool]string{} // keyed by "is v6"
	if c.PGPKey != "" && c.PGPKeyFingerprint != "" {
		pins[keyVersionOf(c.PGPKey) == 6] = c.PGPKeyFingerprint
	}
	if c.PGPKeyV6 != "" && c.PGPKeyV6Fingerprint != "" {
		pins[true] = c.PGPKeyV6Fingerprint
	}
	chosen := map[bool]*wkdCert{}
	continuity := len(pins) == 0
	for _, isV6 := range []bool{false, true} {
		pin, pinned := pins[isV6]
		served := false
		for i := range certs {
			if certs[i].Class.V6 != isV6 {
				continue
			}
			served = true
			if !pinned {
				chosen[isV6] = &certs[i]
				break
			}
			if strings.EqualFold(certs[i].Fingerprint, pin) {
				chosen[isV6], continuity = &certs[i], true
				break
			}
		}
		if pinned && served && chosen[isV6] == nil {
			return nil, nil, nil, true
		}
	}
	use = chosen[false]
	if _, v4Pinned := pins[false]; use == nil || (len(pins) > 0 && !v4Pinned) {
		use = chosen[true]
	}
	return chosen[false], chosen[true], use, !continuity
}

// resolve runs the discovery ladder for one recipient:
//
//  1. A usable key already pinned to a contact wins immediately, silently
//     (tierContactVerified) — no network calls.
//  2. Otherwise, if discover is enabled, try WKD. A validated hit is
//     auto-trusted (tierWKD) and pinned when settings.StoreDiscoveredKeys.
//  3. Otherwise, if discover is enabled, try a public keyserver. A usable
//     hit needs explicit user confirmation before use (tierKeyserverConfirm)
//     — it is not pinned by this call.
//  4. In steps 2/3, if the contact already had a pinned fingerprint that
//     differs from the newly discovered one, the resolver refuses to
//     switch: tierKeyChanged, Usable:false. This only triggers once the
//     previously pinned key has stopped being usable (revoked/expired) and
//     something else now serves a different key for the same address — a
//     still-usable pinned key is trusted as-is in step 1 and discovery
//     never runs.
//  5. Otherwise, tierNone.
func (kr *keyResolver) resolve(ctx context.Context, email string) resolvedKey {
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != strings.TrimSpace(email) {
		return resolvedKey{Tier: tierNone}
	}
	email = parsed.Address
	c, hasContact, err := findContact(kr.store, email)
	if err != nil {
		return resolvedKey{Tier: tierNone, Err: err}
	}
	pinnedFP := ""
	if hasContact && c.PGPKey != "" {
		// A stored key its own pin disputes (e.g. an Autocrypt rotation) is a
		// key change, never a silent encryption target.
		if !keyMatchesPin(c) {
			return resolvedKey{Tier: tierKeyChanged}
		}
		if st, err := pgpmail.CheckKeyStatus(c.PGPKey); err == nil && st.Usable() {
			return resolvedKey{Armored: c.PGPKey, Fingerprint: c.PGPKeyFingerprint, Tier: tierContactVerified, Usable: true, Capability: contactCapability(c)}
		}
		pinnedFP = c.PGPKeyFingerprint
	}
	if !hasContact {
		c = contacts.Contact{}
	}
	if !kr.discover {
		return resolvedKey{Tier: tierNone}
	}

	if kr.suppressed[strings.ToLower(strings.TrimSpace(email))] {
		return resolvedKey{Tier: tierNone}
	}

	if certs, err := fetchWKDKeys(ctx, email); err == nil {
		v4, v6, use, changed := pickWKD(c, certs)
		if changed {
			return resolvedKey{Tier: tierKeyChanged}
		}
		if kr.settings.StoreDiscoveredKeys {
			kr.pin(email, v4, v6, contacts.PGPSourceWKD)
		}
		best := v4
		if v6 != nil && (v4 == nil || v6.Class.rank() > v4.Class.rank()) {
			best = v6
		}
		return resolvedKey{Armored: use.Armored, Fingerprint: use.Fingerprint, Tier: tierWKD, Usable: true, Capability: best.Class.capability()}
	}

	if _, fp, st, err := keyserverLookup(ctx, email); err == nil && st.Usable() {
		if pinnedFP != "" && !strings.EqualFold(pinnedFP, fp) {
			return resolvedKey{Tier: tierKeyChanged}
		}
		return resolvedKey{Fingerprint: fp, Tier: tierKeyserverConfirm}
	}

	return resolvedKey{Tier: tierNone}
}
