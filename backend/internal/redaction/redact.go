package redaction

import (
	"regexp"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

type compiledPattern struct {
	regex       *regexp.Regexp
	replacement string
}

type Engine struct {
	patterns []compiledPattern
}

func New(patterns []config.Pattern) (*Engine, error) {
	compiled := make([]compiledPattern, 0, len(patterns))
	for _, p := range patterns {
		r, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, compiledPattern{regex: r, replacement: p.Replacement})
	}
	return &Engine{patterns: compiled}, nil
}

func (e *Engine) Apply(input string) string {
	result := input
	for _, p := range e.patterns {
		result = p.regex.ReplaceAllString(result, p.replacement)
	}
	return result
}

// ClassifierInput is shared by the poller and model evaluation. Redact before
// clipping so a sensitive value crossing a boundary is still masked in full.
func (e *Engine) ClassifierInput(sender, subject, body string) (string, string, string) {
	clean := func(s string, limit int) string {
		runes := []rune(e.Apply(strings.TrimSpace(s)))
		return string(runes[:min(len(runes), limit)])
	}
	return clean(sender, 256), clean(subject, 512), clean(body, 2000)
}
