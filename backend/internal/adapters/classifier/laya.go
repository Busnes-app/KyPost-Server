package classifier

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// LayaRequest is the bounded choice protocol evaluated before enabling Laya in
// the poller. Inputs must already have passed the shared redaction boundary.
type LayaRequest struct {
	Model      string                  `json:"model"`
	State      map[string]string       `json:"state"`
	Questions  map[string]LayaQuestion `json:"questions"`
	MaxLen     int                     `json:"max_len"`
	HeadMaxLen int                     `json:"head_max_len"`
}

type LayaQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// BuildLayaRequest validates the checkpoint, budgets and option shape. Tokenizer-budget validation is a
// separate rollout gate: character counts cannot prove that policy survives.
func BuildLayaRequest(model, sender, subject, body, instructions string, criteria map[string]string, maxLen, headMaxLen int) (LayaRequest, error) {
	if model != "english" && model != "multilingual" && model != "typed-decisions" {
		return LayaRequest{}, errors.New("laya: select an explicit supported checkpoint")
	}
	if maxLen < 1 || maxLen > 8192 || headMaxLen < 1 || headMaxLen >= maxLen {
		return LayaRequest{}, errors.New("laya: invalid token budgets")
	}
	if len(criteria) < 2 || len(criteria) > 100 || strings.TrimSpace(instructions) == "" {
		return LayaRequest{}, errors.New("laya: choice needs instructions and 2–100 criteria")
	}
	for label, description := range criteria {
		if strings.TrimSpace(label) != label || label == "" || strings.TrimSpace(description) == "" {
			return LayaRequest{}, errors.New("laya: labels and descriptions must be nonempty")
		}
	}
	return LayaRequest{
		Model: model, State: map[string]string{"sender": sender, "subject": subject, "body": body},
		Questions: map[string]LayaQuestion{"category": {Type: "choice", Instructions: instructions, Criteria: criteria}},
		MaxLen:    maxLen, HeadMaxLen: headMaxLen,
	}, nil
}

type LayaDecision struct {
	Choice           string             `json:"choice"`
	Probabilities    map[string]float64 `json:"probabilities"`
	AnswerConfidence float64            `json:"answer_confidence"`
}

// ParseLayaDecision never returns upstream text in errors. A protocol fault is
// not NoAllowedLabelError: the poller treats that error as processed mail.
func ParseLayaDecision(body []byte, request LayaRequest) (LayaDecision, error) {
	var out struct {
		Answers map[string]struct {
			Type             string              `json:"type"`
			Choice           string              `json:"choice"`
			Probabilities    map[string]*float64 `json:"probabilities"`
			AnswerConfidence *float64            `json:"answer_confidence"`
		} `json:"answers"`
		Routing struct {
			Model string `json:"model"`
		} `json:"routing"`
		Usage struct {
			Options map[string]json.RawMessage `json:"options"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &out) != nil {
		return LayaDecision{}, errors.New("laya: malformed response")
	}
	answer, ok := out.Answers["category"]
	criteria := request.Questions["category"].Criteria
	if !ok || answer.Type != "choice" || out.Routing.Model != request.Model || len(criteria) == 0 {
		return LayaDecision{}, errors.New("laya: missing answer or unexpected checkpoint")
	}
	if len(out.Usage.Options) != 0 {
		return LayaDecision{}, errors.New("laya: category options collapsed under the token budget")
	}
	if _, ok := criteria[answer.Choice]; !ok || len(answer.Probabilities) != len(criteria) {
		return LayaDecision{}, errors.New("laya: answer does not match the allowed categories")
	}
	var sum, highest float64
	decision := LayaDecision{Choice: answer.Choice, Probabilities: make(map[string]float64, len(criteria))}
	for label := range criteria {
		p := answer.Probabilities[label]
		if p == nil || *p < 0 || *p > 1 || math.IsNaN(*p) || math.IsInf(*p, 0) {
			return LayaDecision{}, errors.New("laya: invalid probability distribution")
		}
		sum += *p
		highest = max(highest, *p)
		decision.Probabilities[label] = *p
	}
	// Upstream rounds each probability to four decimal places.
	if math.Abs(sum-1) > float64(len(criteria))*0.00005+1e-9 || answer.AnswerConfidence == nil ||
		math.Abs(*answer.AnswerConfidence-highest) > 0.0001 || decision.Probabilities[answer.Choice] != highest {
		return LayaDecision{}, errors.New("laya: inconsistent choice probabilities")
	}
	decision.AnswerConfidence = *answer.AnswerConfidence
	return decision, nil
}
