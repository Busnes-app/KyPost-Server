// Package sorter is the local embedding classifier: a static embedding model
// (potion-base-8M, Model2Vec) turns an email into a vector, and a small
// softmax-regression head trained on shipped examples plus the user's own
// corrections picks a label. Pure computation: no state, no network.
package sorter

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ModelID names the embedding weights a vector was produced by. Stored beside
// every correction so a model upgrade ignores vectors from the old one instead
// of mixing two incompatible spaces.
const ModelID = "minishlab/potion-base-8M@bf8b056651a2c21b8d2565580b8569da283cab23"

// BodyRunes is how much of the (already redacted) body is embedded.
const BodyRunes = 600

const maxSafetensorsHeader = 1 << 20

// Model is potion-base-8M: a WordPiece vocabulary and one row per token.
// An email's vector is the L2-normalised mean of its tokens' rows.
type Model struct {
	vocab    map[string]int32
	unk      int32
	maxChars int
	prefix   string
	dim      int
	rows     []float32
}

// EmailText is the exact text the model was evaluated on:
// "From: <sender>\nSubject: <subject>\n\n<first BodyRunes runes of body>".
func EmailText(sender, subject, body string) string {
	if r := []rune(body); len(r) > BodyRunes {
		body = string(r[:BodyRunes])
	}
	return "From: " + sender + "\nSubject: " + subject + "\n\n" + body
}

// Load reads tokenizer.json and model.safetensors from dir. It refuses a
// tokenizer this package does not implement rather than embedding with the
// wrong one, which would fail silently as poor accuracy.
func Load(dir string) (*Model, error) {
	var tok struct {
		Normalizer struct {
			Type      string `json:"type"`
			Lowercase bool   `json:"lowercase"`
		} `json:"normalizer"`
		PreTokenizer struct {
			Type string `json:"type"`
		} `json:"pre_tokenizer"`
		Model struct {
			Type     string           `json:"type"`
			Unk      string           `json:"unk_token"`
			Prefix   string           `json:"continuing_subword_prefix"`
			MaxChars int              `json:"max_input_chars_per_word"`
			Vocab    map[string]int32 `json:"vocab"`
		} `json:"model"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("read tokenizer: %w", err)
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parse tokenizer: %w", err)
	}
	if tok.Model.Type != "WordPiece" || tok.Normalizer.Type != "BertNormalizer" || !tok.Normalizer.Lowercase ||
		tok.PreTokenizer.Type != "BertPreTokenizer" {
		return nil, errors.New("unsupported tokenizer: want lowercase BERT WordPiece")
	}
	unk, ok := tok.Model.Vocab[tok.Model.Unk]
	if !ok || tok.Model.MaxChars <= 0 {
		return nil, errors.New("tokenizer has no unknown token or word length limit")
	}

	raw, err = os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, fmt.Errorf("read weights: %w", err)
	}
	if len(raw) < 8 {
		return nil, errors.New("weights file truncated")
	}
	n := binary.LittleEndian.Uint64(raw[:8])
	if n > maxSafetensorsHeader || 8+n > uint64(len(raw)) {
		return nil, errors.New("weights header out of range")
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+n], &hdr); err != nil {
		return nil, fmt.Errorf("parse weights header: %w", err)
	}
	var t struct {
		DType   string   `json:"dtype"`
		Shape   []int    `json:"shape"`
		Offsets []uint64 `json:"data_offsets"`
	}
	if err := json.Unmarshal(hdr["embeddings"], &t); err != nil {
		return nil, errors.New("weights have no embeddings tensor")
	}
	if t.DType != "F32" || len(t.Shape) != 2 || len(t.Offsets) != 2 || t.Shape[0] != len(tok.Model.Vocab) || t.Shape[1] <= 0 {
		return nil, errors.New("embeddings tensor does not match the vocabulary")
	}
	body := raw[8+n:]
	size := uint64(t.Shape[0]) * uint64(t.Shape[1]) * 4
	if t.Offsets[0] > t.Offsets[1] || t.Offsets[1] > uint64(len(body)) || t.Offsets[1]-t.Offsets[0] != size {
		return nil, errors.New("embeddings tensor out of range")
	}
	data := body[t.Offsets[0]:t.Offsets[1]]
	rows := make([]float32, t.Shape[0]*t.Shape[1])
	for i := range rows {
		rows[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	for id := range tok.Model.Vocab {
		if int(tok.Model.Vocab[id]) >= t.Shape[0] || tok.Model.Vocab[id] < 0 {
			return nil, errors.New("vocabulary id outside the embeddings tensor")
		}
	}
	return &Model{vocab: tok.Model.Vocab, unk: unk, maxChars: tok.Model.MaxChars, prefix: tok.Model.Prefix,
		dim: t.Shape[1], rows: rows}, nil
}

// Dim is the vector length.
func (m *Model) Dim() int { return m.dim }

// Embed returns the unit-length vector for text (all zeros if no known token).
func (m *Model) Embed(text string) []float32 {
	sum := make([]float64, m.dim)
	n := 0
	for _, id := range m.Tokenize(text) {
		if id == m.unk {
			continue
		}
		row := m.rows[int(id)*m.dim : int(id+1)*m.dim]
		for j, v := range row {
			sum[j] += float64(v)
		}
		n++
	}
	// Mean then normalise == normalise the sum; n only matters for the zero case.
	var norm2 float64
	for _, v := range sum {
		norm2 += v * v
	}
	out := make([]float32, m.dim)
	if n == 0 || norm2 == 0 {
		return out
	}
	inv := 1 / math.Sqrt(norm2)
	for j, v := range sum {
		out[j] = float32(v * inv)
	}
	return out
}

// Tokenize reproduces Hugging Face's BertNormalizer (clean text, pad CJK,
// strip accents, lowercase) + BertPreTokenizer + WordPiece, without special
// tokens. Pinned against the reference tokenizer by testdata/parity.json.
func (m *Model) Tokenize(text string) []int32 {
	var ids []int32
	for _, word := range preTokenize(normalize(text)) {
		ids = append(ids, m.wordPiece(word)...)
	}
	return ids
}

func normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0 || r == unicode.ReplacementChar || isControl(r):
		case r == '\t' || r == '\n' || r == '\r' || unicode.IsSpace(r):
			b.WriteByte(' ')
		case isCJK(r):
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	var out strings.Builder
	for _, r := range norm.NFD.String(b.String()) {
		if !unicode.Is(unicode.Mn, r) {
			out.WriteRune(r)
		}
	}
	return strings.ToLower(out.String())
}

// isControl matches the reference's "is_other" test, minus unassigned code
// points. ponytail: Go has no Cn table; an unassigned rune survives here where
// the reference drops it, which only matters for text containing unassigned
// code points.
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) || (r >= 0x2B740 && r <= 0x2B81F) || (r >= 0x2B920 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) || (r >= 0x2F800 && r <= 0x2FA1F)
}

func isPunct(r rune) bool {
	if r < 128 {
		return (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126)
	}
	return unicode.IsPunct(r)
}

func preTokenize(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunct(r):
			flush()
			words = append(words, string(r))
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

func (m *Model) wordPiece(word string) []int32 {
	runes := []rune(word)
	if len(runes) > m.maxChars {
		return []int32{m.unk}
	}
	var ids []int32
	for start := 0; start < len(runes); {
		end, found := len(runes), int32(-1)
		for ; start < end; end-- {
			sub := string(runes[start:end])
			if start > 0 {
				sub = m.prefix + sub
			}
			if id, ok := m.vocab[sub]; ok {
				found = id
				break
			}
		}
		if found < 0 {
			return []int32{m.unk}
		}
		ids = append(ids, found)
		start = end
	}
	return ids
}
