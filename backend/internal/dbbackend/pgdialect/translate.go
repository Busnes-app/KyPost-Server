// Package pgdialect runs the store's SQLite-dialect SQL on Postgres: a small statement
// translator and the kypg database/sql driver that applies it and serialises writers.
package pgdialect

import (
	"strconv"
	"strings"
)

// Translate rewrites one SQLite-dialect statement (or script) for Postgres. It knows
// exactly these rules, applied outside string literals, quoted identifiers and comments:
//   - ? placeholders become $1..$n, and ?NNN becomes $NNN (a later bare ? continues from
//     the largest number so far, as SQLite numbers it);
//   - INSERT OR IGNORE INTO t ... becomes INSERT INTO t ... ON CONFLICT DO NOTHING (before
//     a top-level RETURNING, otherwise at the end of the statement);
//   - unixepoch() becomes the current statement's time in whole seconds;
//   - LIKE becomes ILIKE: SQLite's LIKE folds ASCII case, and ILIKE does the same on a C-locale
//     database, so every LIKE keeps its meaning (ESCAPE clauses are unchanged);
//   - instr( becomes strpos( (same arguments, same 1-based result, 0 when absent);
//   - the random identifier idiom lower(hex(randomblob(16))) becomes 32 random hex digits.
//
// Anything else that differs is written out at the call site for each backend. This is
// not a parser: keep it to rules that are true token by token.
func Translate(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 16)
	n, depth := 0, 0
	ignore := false // inside an INSERT OR IGNORE statement, owed its ON CONFLICT clause
	flush := func() {
		if ignore {
			trimmed := strings.TrimRight(b.String(), " \t\r\n")
			b.Reset()
			b.WriteString(trimmed)
			b.WriteString(" ON CONFLICT DO NOTHING")
			ignore = false
		}
	}
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(q) {
				if q[j] == c {
					if j+1 < len(q) && q[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			j = min(j+1, len(q))
			b.WriteString(q[i:j])
			i = j
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q) - i
			}
			b.WriteString(q[i : i+j])
			i += j
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				j = len(q) - i - 2
			} else {
				j += 2
			}
			b.WriteString(q[i : i+2+j])
			i += 2 + j
		case c == '$' && strings.HasPrefix(q[i:], "$$"):
			// A $$-quoted body (a plpgsql function in Postgres-only DDL) is copied as is.
			j := strings.Index(q[i+2:], "$$")
			if j < 0 {
				j = len(q) - i - 4
			}
			b.WriteString(q[i : i+4+j])
			i += 4 + j
		case c == '?':
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			if j > i+1 {
				// ?NNN is parameter NNN, and a bare ? after it is the largest so far plus one.
				k, _ := strconv.Atoi(q[i+1 : j])
				n = max(n, k)
				b.WriteString("$" + q[i+1:j])
				i = j
				continue
			}
			n++
			b.WriteString("$" + strconv.Itoa(n))
			i++
		case c == '(':
			depth++
			b.WriteByte(c)
			i++
		case c == ')':
			depth--
			b.WriteByte(c)
			i++
		case c == ';':
			if depth == 0 {
				flush()
			}
			b.WriteByte(c)
			i++
		case isWordStart(c) && (i == 0 || !isWordByte(q[i-1])):
			j := i
			for j < len(q) && isWordByte(q[j]) {
				j++
			}
			word := q[i:j]
			switch strings.ToUpper(word) {
			case "INSERT":
				if rest, ok := skipWords(q[j:], "OR", "IGNORE"); ok {
					b.WriteString(word)
					ignore = true
					i = len(q) - len(rest)
					continue
				}
			case "RETURNING":
				if depth == 0 && ignore {
					flush()
					b.WriteByte(' ')
				}
			case "LIKE":
				b.WriteString("ILIKE")
				i = j
				continue
			case "INSTR":
				if strings.HasPrefix(q[j:], "(") {
					b.WriteString("strpos")
					i = j
					continue
				}
			case "LOWER":
				if rest, ok := skipWords(q[j:], "(", "hex", "(", "randomblob", "(", "16", ")", ")", ")"); ok {
					b.WriteString("replace(gen_random_uuid()::text,'-','')")
					i = len(q) - len(rest)
					continue
				}
			case "UNIXEPOCH":
				if rest, ok := skipWords(q[j:], "(", ")"); ok {
					b.WriteString("(floor(extract(epoch from statement_timestamp()))::bigint)")
					i = len(q) - len(rest)
					continue
				}
			}
			b.WriteString(word)
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	if depth == 0 {
		flush()
	}
	return b.String()
}

// skipWords consumes the given tokens (words compared without case, or single
// punctuation), each optionally preceded by whitespace, and returns what follows.
func skipWords(s string, tokens ...string) (string, bool) {
	for _, t := range tokens {
		s = strings.TrimLeft(s, " \t\r\n")
		if len(s) < len(t) || !strings.EqualFold(s[:len(t)], t) {
			return "", false
		}
		if isWordStart(t[0]) && len(s) > len(t) && isWordByte(s[len(t)]) {
			return "", false
		}
		s = s[len(t):]
	}
	return s, true
}

func isWordStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isWordByte(c byte) bool { return isWordStart(c) || c >= '0' && c <= '9' || c == '$' }

// Split cuts a script into its statements at top-level semicolons (outside literals,
// quoted identifiers, comments and $$ bodies) and counts each one's ? placeholders.
// Postgres takes several statements in one call only without arguments, while the
// store's SQLite scripts sometimes bind them; the driver runs such a script piece by piece.
func Split(q string) (stmts []string, params []int) {
	start, n := 0, 0
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '\'' || c == '"':
			for i++; i < len(q) && q[i] != c; i++ {
			}
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			if j := strings.Index(q[i+2:], "*/"); j >= 0 {
				i += j + 3
			} else {
				i = len(q)
			}
		case c == '$' && strings.HasPrefix(q[i:], "$$"):
			if j := strings.Index(q[i+2:], "$$"); j >= 0 {
				i += j + 3
			} else {
				i = len(q)
			}
		case c == '?':
			n++
		case c == ';':
			if strings.TrimSpace(q[start:i]) != "" {
				stmts, params = append(stmts, q[start:i]), append(params, n)
			}
			start, n = i+1, 0
		}
	}
	if strings.TrimSpace(q[start:]) != "" {
		stmts, params = append(stmts, q[start:]), append(params, n)
	}
	return stmts, params
}

// IsRead reports whether a statement only reads: it starts with SELECT, VALUES, SHOW or
// EXPLAIN. Everything else, a WITH included, is treated as a write.
func IsRead(q string) bool {
	for {
		q = strings.TrimLeft(q, " \t\r\n(")
		switch {
		case strings.HasPrefix(q, "--"):
			if i := strings.IndexByte(q, '\n'); i >= 0 {
				q = q[i:]
				continue
			}
			return false
		case strings.HasPrefix(q, "/*"):
			if i := strings.Index(q, "*/"); i >= 0 {
				q = q[i+2:]
				continue
			}
			return false
		}
		break
	}
	j := 0
	for j < len(q) && isWordByte(q[j]) {
		j++
	}
	switch strings.ToUpper(q[:j]) {
	case "SELECT", "VALUES", "SHOW", "EXPLAIN":
		return true
	}
	return false
}
