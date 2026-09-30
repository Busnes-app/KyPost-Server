// Package rules implements KyPost's filter-rules engine: a structured
// Rule model shared by a graphical builder and a hand-rolled Sieve-subset
// text editor (sieve.go), evaluated by engine.go both automatically (the
// poller, on new mail) and on demand (a manual "run rules now" backfill).
package rules

// Rule is one filter rule: a match condition tree plus an ordered list of
// actions to run when it matches. Rev/CreatedAt/UpdatedAt are stamped by
// Store.Upsert, never set directly by callers.
type Rule struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	Order     int        `json:"order"`
	Scope     RuleScope  `json:"scope"`
	Match     MatchGroup `json:"match"`
	Actions   []Action   `json:"actions"`
	Rev       int64      `json:"rev"`
	CreatedAt string     `json:"createdAt"`
	UpdatedAt string     `json:"updatedAt"`
}

// RuleScope restricts which mailbox(es) a rule applies to. An empty Folders
// list means "all folders" for a manual run; the automatic poller trigger is
// inherently INBOX-only regardless of Scope.
type RuleScope struct {
	Folders []string `json:"folders,omitempty"`
}

// MatchGroup is a boolean group of Conditions, combined with Op ("allof" —
// AND — or "anyof" — OR).
type MatchGroup struct {
	Op         string      `json:"op"`
	Conditions []Condition `json:"conditions"`
}

// Condition is one leaf test, or (when Group is set) a nested boolean group.
// The GUI builder only ever constructs one flat allof/anyof of leaf
// conditions (Group always nil); a Match tree with Group set anywhere below
// the root came from hand-edited Sieve and is reported to the frontend as
// not GUI-editable (see rules_handlers.go's guiEditable computation).
type Condition struct {
	Negate bool        `json:"negate,omitempty"`
	Group  *MatchGroup `json:"group,omitempty"`
	// Field is one of "from"|"to"|"cc"|"bcc"|"subject"|"body"|"keyword"|"header".
	// Ignored when Group is set.
	Field string `json:"field,omitempty"`
	// Header names the header a "header" Field tests, e.g. "X-Spam-Flag".
	Header string `json:"header,omitempty"`
	// Comparator is one of "contains"|"is"|"matches"|"regex", or the
	// engine/sieve-internal "exists" value (field non-empty, only valid for
	// the 5 header fields — see sieve.go's exists-test mapping). Ignored
	// when Group is set.
	Comparator string `json:"comparator,omitempty"`
	Value      string `json:"value,omitempty"`
}

// Action is one effect to apply when a rule matches.
// Type is one of "keyword"|"unkeyword"|"move"|"read"|"archive"|"spam"|
// "delete"|"stop". Value holds the keyword name for keyword/unkeyword or
// the target folder for move; unused for the other types.
type Action struct {
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}
