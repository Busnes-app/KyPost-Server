package api

import (
	"encoding/base64"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpautocrypt"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

// buildAutocryptHeader returns the value for an outbound "Autocrypt:" header
// advertising the sender's own public key: "addr=<from>; keydata=<base64>",
// where keydata is the minimal binary key (pgpmail.AutocryptKeydata). It
// returns ok=false — meaning "do not advertise" — when there is no key, the key
// does not parse or is unusable, the send address is not carried as a user ID
// on the key (Autocrypt requires addr to equal the From address), or the header
// would exceed pgpautocrypt.MaxHeaderBytes. No prefer-encrypt is emitted.
func buildAutocryptHeader(pubKeyArmored, fromAddr string) (string, bool) {
	target := strings.ToLower(strings.TrimSpace(fromAddr))
	if strings.TrimSpace(pubKeyArmored) == "" || target == "" {
		return "", false
	}
	binary, err := pgpmail.AutocryptKeydata(pubKeyArmored, target)
	if err != nil {
		return "", false
	}
	header := "addr=" + target + "; keydata=" + base64.StdEncoding.EncodeToString(binary)
	if len(header) > pgpautocrypt.MaxHeaderBytes {
		return "", false
	}
	return header, true
}

// outboundAutocryptHeader computes the value for an outbound "Autocrypt:"
// header advertising userID's own public key at envelopeFrom, degrading to
// "" (do not advertise — never error the send) when the user has no PGP
// public key, per-user discovery settings fail to load, advertising is
// disabled (pgpdiscovery.Settings.AdvertiseAutocrypt), or the key/address
// combination is rejected by buildAutocryptHeader.
func (s *Server) outboundAutocryptHeader(userID, envelopeFrom string) string {
	u, uerr := s.users.Get(userID)
	if uerr != nil || u.PGPPublicKey == "" {
		return ""
	}
	settings, serr := pgpdiscovery.Load(s.userStateDir(userID))
	if serr != nil || !settings.AdvertiseAutocrypt {
		return ""
	}
	if v, ok := buildAutocryptHeader(u.PGPPublicKey, envelopeFrom); ok {
		return v
	}
	return ""
}
