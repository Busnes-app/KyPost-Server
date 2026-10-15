package rules

import "testing"

// FuzzParseRuleText feeds user-supplied Sieve text to the parser. Any script it
// accepts must survive CompileRule and come back unchanged: the stored rule is
// shown to the user as CompileRule's output and may be saved again.
func FuzzParseRuleText(f *testing.F) {
	for _, r := range roundTripFixtures {
		src, err := CompileRule(r)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(src)
	}
	f.Add("# c\nif allof(header :contains [\"from\"] \"acme\") { /* b */ fileinto \"X\"; }\n")
	f.Add("if allof(header :is [\"from\", \"to\"] \"a\\\"b\\\\\") {\n keep;\n}\n")

	f.Fuzz(func(t *testing.T, script string) {
		parsed, err := ParseRuleText(script, Rule{})
		if err != nil {
			return
		}
		first, err := CompileRule(parsed)
		if err != nil {
			t.Fatalf("parsed rule does not compile: %v\nscript: %q", err, script)
		}
		reparsed, err := ParseRuleText(first, Rule{})
		if err != nil {
			t.Fatalf("compiled rule does not parse: %v\ncompiled: %q", err, first)
		}
		second, err := CompileRule(reparsed)
		if err != nil || second != first {
			t.Fatalf("round trip changed the rule (err %v):\nfirst:  %q\nsecond: %q", err, first, second)
		}
	})
}
