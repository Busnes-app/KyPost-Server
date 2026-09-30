package rules

import (
	"context"
	"reflect"
	"testing"
)

func headerRule(cond Condition) Rule {
	return Rule{Name: "r", Enabled: true, Match: MatchGroup{Op: "allof", Conditions: []Condition{cond}}}
}

func TestEvaluate_HeaderConditionMatchesAnyValue(t *testing.T) {
	// A provider appends its own verdict below any copy the sender forged, so
	// every copy must be tested, not just the first.
	in := EvalInput{Headers: map[string][]string{"X-Spam-Flag": {"NO", "YES"}}}
	rule := headerRule(Condition{Field: "header", Header: "x-spam-flag", Comparator: "is", Value: "yes"})
	if got := Evaluate(context.Background(), in, []Rule{rule}); len(got.Matched) != 1 {
		t.Fatalf("want match on second header copy, got %+v", got)
	}
	absent := EvalInput{Headers: map[string][]string{}}
	if got := Evaluate(context.Background(), absent, []Rule{rule}); len(got.Matched) != 0 {
		t.Fatalf("absent header matched: %+v", got)
	}
	exists := headerRule(Condition{Field: "header", Header: "X-Spam-Flag", Comparator: "exists"})
	if got := Evaluate(context.Background(), in, []Rule{exists}); len(got.Matched) != 1 {
		t.Fatalf("exists did not match present header: %+v", got)
	}
}

func TestEvaluate_HeaderConditionUnevaluableWhenNotFetched(t *testing.T) {
	// Nil Headers means the caller never fetched them; under Negate a plain
	// "false" would become "matches every message".
	rule := headerRule(Condition{Field: "header", Header: "X-Spam-Flag", Comparator: "is", Value: "YES", Negate: true})
	if got := Evaluate(context.Background(), EvalInput{}, []Rule{rule}); len(got.Matched) != 0 {
		t.Fatalf("negated header condition matched without fetched headers: %+v", got)
	}
	fetched := EvalInput{Headers: map[string][]string{}}
	if got := Evaluate(context.Background(), fetched, []Rule{rule}); len(got.Matched) != 1 {
		t.Fatalf("negated condition on absent header should match once headers were fetched: %+v", got)
	}
}

// Unknown must survive an enclosing "not": not allof(<unevaluable>) is still
// unknown, never true.
func TestEvaluate_UnknownSurvivesNestedNegation(t *testing.T) {
	nested := Rule{Name: "r", Enabled: true, Match: MatchGroup{Op: "allof", Conditions: []Condition{{
		Negate: true,
		Group: &MatchGroup{Op: "allof", Conditions: []Condition{
			{Field: "header", Header: "X-Spam-Flag", Comparator: "exists"},
		}},
	}}}}
	if got := Evaluate(context.Background(), EvalInput{}, []Rule{nested}); len(got.Matched) != 0 {
		t.Fatalf("not(group of unfetched header) matched: %+v", got)
	}
	if got := Evaluate(context.Background(), EvalInput{Headers: map[string][]string{}}, []Rule{nested}); len(got.Matched) != 1 {
		t.Fatalf("with headers fetched and absent, not(exists) should match: %+v", got)
	}
	// anyof with a definite true still matches despite an unknown sibling.
	anyof := Rule{Name: "a", Enabled: true, Match: MatchGroup{Op: "anyof", Conditions: []Condition{
		{Field: "header", Header: "X-Spam-Flag", Comparator: "exists"},
		{Field: "subject", Comparator: "contains", Value: "hi"},
	}}}
	if got := Evaluate(context.Background(), EvalInput{Subject: "hi"}, []Rule{anyof}); len(got.Matched) != 1 {
		t.Fatalf("anyof with a true branch did not match: %+v", got)
	}
}

func TestMatchGroup_CancelledContextIsUnknownUnderNegation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := MatchGroup{Op: "allof", Conditions: []Condition{{Negate: true, Group: &MatchGroup{Op: "allof", Conditions: []Condition{{Field: "subject", Comparator: "contains", Value: "x"}}}}}}
	if m, known := matchGroup(ctx, g, EvalInput{Subject: "x"}); m || known {
		t.Fatalf("matchGroup on cancelled ctx = (%v, %v), want (false, false)", m, known)
	}
}

func TestHeaderNamesCanonicalAndEnabledOnly(t *testing.T) {
	disabled := headerRule(Condition{Field: "header", Header: "X-Ignored", Comparator: "exists"})
	disabled.Enabled = false
	nested := Rule{Enabled: true, Match: MatchGroup{Op: "anyof", Conditions: []Condition{
		{Group: &MatchGroup{Op: "allof", Conditions: []Condition{{Field: "header", Header: "x-rspamd-score", Comparator: "contains", Value: "1"}}}},
		{Field: "header", Header: "X-Spam-Flag", Comparator: "exists"},
		{Field: "from", Comparator: "contains", Value: "a"},
	}}}
	got := HeaderNames([]Rule{disabled, nested, headerRule(Condition{Field: "header", Header: "x-spam-flag", Comparator: "exists"})})
	if want := []string{"X-Rspamd-Score", "X-Spam-Flag"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HeaderNames = %v, want %v", got, want)
	}
}

func TestParseRuleText_HeaderTests(t *testing.T) {
	r, err := ParseRuleText(`if anyof(header :is ["X-Spam-Flag"] "YES", exists ["X-Rspamd-Action"]) { stop; }`, Rule{})
	if err != nil {
		t.Fatalf("ParseRuleText: %v", err)
	}
	want := []Condition{
		{Field: "header", Header: "X-Spam-Flag", Comparator: "is", Value: "YES"},
		{Field: "header", Header: "X-Rspamd-Action", Comparator: "exists"},
	}
	if !reflect.DeepEqual(r.Match.Conditions, want) {
		t.Fatalf("conditions = %+v, want %+v", r.Match.Conditions, want)
	}
	for _, bad := range []string{
		`if address :is ["X-Spam-Flag"] "YES" { stop; }`, // address tests stay on address fields
		`if header :is ["X Spam"] "YES" { stop; }`,
		`if exists ["X-Spam)"] { stop; }`,
	} {
		if _, err := ParseRuleText(bad, Rule{}); err == nil {
			t.Errorf("ParseRuleText(%q) = nil error, want rejection", bad)
		}
	}
}

func TestValidateMatchShapeRejectsUnsafeHeaderName(t *testing.T) {
	for _, name := range []string{"", "X-A\r\nA1 LOGOUT", "X-A)", "body", "Keyword", "From"} {
		m := MatchGroup{Op: "allof", Conditions: []Condition{{Field: "header", Header: name, Comparator: "exists"}}}
		if err := ValidateMatchShape(m); err == nil {
			t.Errorf("ValidateMatchShape accepted header name %q", name)
		}
	}
}
