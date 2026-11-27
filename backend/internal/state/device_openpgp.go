package state

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

var ErrInvalidOpenPGP = errors.New("openpgp must be a nonempty list of v4, v6 or rfc9980")

// NormalizeOpenPGP validates the device's report, never inferring OpenPGP support
// from its enrollment transport. Missing reports are legacy v4, including after
// a client downgrade. Canonical order matches the cross-client wire contract.
func NormalizeOpenPGP(values []string) ([]string, error) {
	if values == nil {
		return []string{"v4"}, nil
	}
	if len(values) == 0 || len(values) > 16 {
		return nil, ErrInvalidOpenPGP
	}
	seen := make(map[string]bool)
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !slices.Contains([]string{"v4", "v6", "rfc9980"}, value) {
			return nil, ErrInvalidOpenPGP
		}
		seen[value] = true
	}
	out := []string{}
	for _, value := range []string{"v4", "v6", "rfc9980"} {
		if seen[value] {
			out = append(out, value)
		}
	}
	return out, nil
}

func encodeOpenPGP(values []string) (string, error) {
	values, err := NormalizeOpenPGP(values)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(values)
	return string(data), err
}

func decodeOpenPGP(encoded string) ([]string, error) {
	var values []string
	if err := json.Unmarshal([]byte(encoded), &values); err != nil {
		return nil, err
	}
	return NormalizeOpenPGP(values)
}

// Other writers (push registration/marker changes) preserve the last report.
// The two OpenPGP device routes explicitly pass a report, even when omitted.
func optionalOpenPGP(reports [][]string) (*string, error) {
	if len(reports) == 0 {
		return nil, nil
	}
	encoded, err := encodeOpenPGP(reports[0])
	return &encoded, err
}
