package processor

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sorter"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// DefaultEmbedMinConfidence is the probability at which the embedding sorter's
// answer is applied without asking the LLM.
const DefaultEmbedMinConfidence = 0.6

// sortGuess is the embedding sorter's view of one message: its vector (kept so
// a later correction can be learned) and, when a head exists, its best label
// among the allowed ones.
type sortGuess struct {
	vec   []float32
	label string
	conf  float64
}

func (g sortGuess) confident(min float64) bool { return g.label != "" && g.conf >= min }

type cachedHead struct {
	key  string
	head *sorter.Head
}

// SetSorter enables the hybrid engine: m answers when it is at least
// minConfidence sure, the LLM answers otherwise. nil keeps LLM-only operation.
func (p *Poller) SetSorter(m *sorter.Model, minConfidence float64) {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	p.embed, p.embedMin, p.heads = m, minConfidence, map[string]*cachedHead{}
}

// sorterTrainBudget bounds one training run in wall-clock time. Training runs
// on the tick goroutine of a daemon every account shares, so this — together
// with sorter.MaxLabels/MaxExamples, which bound the work itself — is what one
// account can cost the others. Measured worst case at the limits is ~1 s.
const sorterTrainBudget = 3 * time.Second

// userHead returns this user's trained head, retraining only when their
// corrections, label list or label descriptions changed. nil means LLM only:
// the sorter is off, the account is over the training budget (more than
// sorter.MaxLabels labels, or a run that outlasts sorterTrainBudget), or the
// corrections cannot be read — training on a partial read would silently forget
// what the user taught, and the LLM still sorts. A budget refusal is cached
// under the same key as a head, so it is not retried until the inputs change.
func (p *Poller) userHead(ctx context.Context, userID string, store *state.Store, labels config.UserLabelSettings) *sorter.Head {
	p.userMu.Lock()
	m, cached := p.embed, p.heads[userID]
	p.userMu.Unlock()
	if m == nil || len(labels.Allowlist) == 0 {
		return nil
	}
	gen, err := store.SorterGeneration()
	if err != nil {
		p.log.Error("cannot read sorter generation; LLM only this tick", "user_id", userID, "error", err.Error())
		return nil
	}
	descKeys := slices.Sorted(maps.Keys(labels.Descriptions))
	var key strings.Builder
	key.WriteString(gen + "\x00" + strings.Join(labels.Allowlist, "\x01"))
	for _, k := range descKeys {
		key.WriteString("\x00" + k + "\x01" + labels.Descriptions[k])
	}
	if cached != nil && cached.key == key.String() {
		return cached.head
	}
	remember := func(head *sorter.Head) *sorter.Head {
		p.userMu.Lock()
		p.heads[userID] = &cachedHead{key: key.String(), head: head}
		p.userMu.Unlock()
		return head
	}
	// Checked before any embedding work: a label list is user-controlled and
	// unbounded, and every label is a class the head must fit.
	if len(labels.Allowlist) > sorter.MaxLabels {
		p.log.Info("embedding sorter skipped: too many labels; LLM only", "user_id", userID,
			"allowlist_count", strconv.Itoa(len(labels.Allowlist)), "limit", strconv.Itoa(sorter.MaxLabels))
		return remember(nil)
	}
	examples := m.SeedExamples(labels.Allowlist, labels.Descriptions)
	corrections, err := store.SorterCorrectionsStrict(sorter.ModelID, max(sorter.MaxExamples-len(examples), 0))
	if err != nil {
		p.log.Error("cannot read sorter corrections; LLM only this tick", "user_id", userID, "error", err.Error())
		return nil
	}
	for _, c := range corrections {
		if slices.Contains(labels.Allowlist, c.Label) && len(c.Vec) == m.Dim() {
			examples = append(examples, sorter.Example{Label: c.Label, Weight: 1, Vec: c.Vec})
		}
	}
	var prev *sorter.Head
	if cached != nil {
		prev = cached.head
	}
	trainCtx, cancel := context.WithTimeout(ctx, sorterTrainBudget)
	defer cancel()
	head, err := sorter.Train(trainCtx, examples, prev)
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutdown or tick timeout, not a verdict on this account's inputs
		}
		p.log.Error("embedding sorter training over budget; LLM only", "user_id", userID, "error", err.Error())
		return remember(nil)
	}
	return remember(head)
}

// guess embeds the message exactly as the LLM would see it — redacted and
// clamped — and asks the head for a label among allowlist. Memoised per tick so
// the rate-limit pre-check in tickUser and handleMessage share one embedding.
func (p *Poller) guess(uc userCtx, msg imapadapter.Message, allowlist []string) sortGuess {
	if g, ok := uc.guesses[msg.ID]; ok {
		return g
	}
	p.userMu.Lock()
	m := p.embed
	p.userMu.Unlock()
	var g sortGuess
	if m != nil {
		red := p.currentRedaction()
		body := truncateRunes(red.Apply(strings.TrimSpace(msg.Body)), maxClassifyBodyRunes)
		sender := truncateRunes(red.Apply(strings.TrimSpace(msg.Sender)), maxClassifySenderRunes)
		subject := truncateRunes(red.Apply(strings.TrimSpace(msg.Subject)), maxClassifySubjectRunes)
		g.vec = m.Embed(sorter.EmailText(sender, subject, body))
		g.label, g.conf = uc.head.Predict(g.vec, allowlist)
	}
	if uc.guesses != nil {
		uc.guesses[msg.ID] = g
	}
	return g
}

// rememberPrediction records what was applied to msg so a later relabel by the
// user can be learned. Best effort: losing one prediction loses one possible
// lesson, never mail, so a failure is logged and the message carries on.
func (p *Poller) rememberPrediction(uc userCtx, msg imapadapter.Message, g sortGuess, applied string) {
	if g.vec == nil || applied == "" {
		return
	}
	if err := uc.store.RecordSorterPrediction(msg.ID, state.SorterCheck(msg.Sender, msg.Subject),
		sorter.ModelID, g.vec, applied); err != nil {
		p.log.Error("cannot record sorter prediction", "user_id", uc.id, "message_id", msg.ID, "error", err.Error())
	}
}
