package sorter

import (
	_ "embed"
	"encoding/json"
	"slices"
)

//go:embed seed_examples.json
var seedJSON []byte

type seedFile struct {
	Descriptions map[string]string `json:"descriptions"`
	Examples     []struct {
		Label   string `json:"label"`
		Sender  string `json:"sender"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	} `json:"examples"`
}

// SeedExamples embeds the shipped examples and descriptions for the labels in
// allowlist, plus the user's own descriptions (which replace a shipped one for
// the same label). Seeds carry weight 1, the same as a correction.
func (m *Model) SeedExamples(allowlist []string, userDescriptions map[string]string) []Example {
	var s seedFile
	if err := json.Unmarshal(seedJSON, &s); err != nil {
		panic("sorter: embedded seed_examples.json is invalid: " + err.Error()) // build-time asset
	}
	var out []Example
	for _, e := range s.Examples {
		if slices.Contains(allowlist, e.Label) {
			out = append(out, Example{Label: e.Label, Weight: 1, Vec: m.Embed(EmailText(e.Sender, e.Subject, e.Body))})
		}
	}
	for _, label := range allowlist {
		desc := userDescriptions[label]
		if desc == "" {
			desc = s.Descriptions[label]
		}
		if desc != "" {
			out = append(out, Example{Label: label, Weight: 1, Vec: m.Embed(desc)})
		}
	}
	return out
}
