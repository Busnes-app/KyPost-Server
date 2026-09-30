package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/redaction"
)

type providerEvalOptions struct {
	Provider, Model, Base, CorpusPath, TuningPath, RubricPath, EnvironmentPath, Output string
	Revision                                                                           string
	Timeout                                                                            time.Duration
	DryRun                                                                             bool
}

type layaRubric struct {
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
	MaxLen       int               `json:"max_len"`
	HeadMaxLen   int               `json:"head_max_len"`
}

type providerResult struct {
	ID                string             `json:"id"`
	Bucket            string             `json:"bucket"`
	Gold              string             `json:"gold"`
	NeedsAdjudication bool               `json:"needs_adjudication"`
	Label             string             `json:"label"`
	Correct           bool               `json:"correct"`
	InjectionResisted bool               `json:"injection_resisted,omitempty"`
	LatencyMS         int64              `json:"latency_ms"`
	Probabilities     map[string]float64 `json:"probabilities,omitempty"`
	AnswerConfidence  float64            `json:"answer_confidence,omitempty"`
	Error             string             `json:"error,omitempty"`
}

type providerSummary struct {
	Total               int                       `json:"total"`
	Correct             int                       `json:"correct"`
	Errors              int                       `json:"errors"`
	PendingAdjudication int                       `json:"pending_adjudication"`
	SettledTotal        int                       `json:"settled_total"`
	SettledCorrect      int                       `json:"settled_correct"`
	MacroF1             float64                   `json:"provisional_macro_f1"`
	P50MS               int64                     `json:"p50_ms"`
	P95MS               int64                     `json:"p95_ms"`
	Confusion           map[string]map[string]int `json:"provisional_confusion"`
}

func runProviderEvaluation(ctx context.Context, o providerEvalOptions) error {
	if o.Provider != "ollama" && o.Provider != "laya" {
		return errors.New("provider must be historical, ollama, or laya")
	}
	if err := classifier.ValidateBaseURL(o.Base); err != nil {
		return err
	}
	if o.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if o.Model == "" {
		o.Model = classifier.DefaultModel
		if o.Provider == "laya" {
			o.Model = "english"
		}
	}
	corpusBytes, err := os.ReadFile(o.CorpusPath)
	if err != nil {
		return err
	}
	var corpus corpusFile
	if err := json.Unmarshal(corpusBytes, &corpus); err != nil {
		return err
	}
	tuning, err := os.ReadFile(o.TuningPath)
	if err != nil {
		return err
	}
	labels := classifier.ParseAllowedLabels(string(tuning))
	if len(labels) == 0 || len(corpus.Emails) == 0 {
		return errors.New("empty allowlist or corpus")
	}
	var rubric layaRubric
	if o.Provider == "laya" {
		b, err := os.ReadFile(o.RubricPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &rubric); err != nil {
			return err
		}
		if len(rubric.Criteria) != len(labels) {
			return errors.New("rubric must match the tuning allowlist")
		}
		for _, label := range labels {
			if _, ok := rubric.Criteria[label]; !ok {
				return errors.New("rubric must match the tuning allowlist")
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range corpus.Emails {
		if e.ID == "" || seen[e.ID] {
			return errors.New("corpus requires unique nonempty IDs")
		}
		seen[e.ID] = true
		if !slices.Contains(labels, e.Gold) {
			return errors.New("corpus gold label is outside allowlist")
		}
	}
	patterns := config.Default().Redaction.Patterns
	red, err := redaction.New(patterns)
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: o.Timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("evaluation refuses redirects") }}
	report := struct {
		Provider             string           `json:"provider"`
		Model                string           `json:"model"`
		Started              time.Time        `json:"started"`
		GoVersion            string           `json:"go_version"`
		Arch                 string           `json:"arch"`
		CorpusSHA256         string           `json:"corpus_sha256"`
		TuningSHA256         string           `json:"tuning_sha256"`
		Patterns             []config.Pattern `json:"redaction_patterns"`
		Rubric               *layaRubric      `json:"rubric,omitempty"`
		Environment          json.RawMessage  `json:"environment,omitempty"`
		ServiceMetadata      json.RawMessage  `json:"service_metadata,omitempty"`
		FinalServiceMetadata json.RawMessage  `json:"final_service_metadata,omitempty"`
		WarmupMS             int64            `json:"warmup_ms"`
		Complete             bool             `json:"complete"`
		Summary              providerSummary  `json:"summary"`
		Results              []providerResult `json:"results"`
	}{Provider: o.Provider, Model: o.Model, Started: time.Now().UTC(), GoVersion: runtime.Version(), Arch: runtime.GOARCH,
		CorpusSHA256: sha256Hex(corpusBytes), TuningSHA256: sha256Hex(tuning), Patterns: patterns}
	if o.EnvironmentPath != "" {
		b, err := os.ReadFile(o.EnvironmentPath)
		if err != nil {
			return err
		}
		if !json.Valid(b) {
			return errors.New("environment manifest must be valid JSON")
		}
		report.Environment = b
	}
	var ollama *classifier.HTTPClient
	var identity string
	if o.Provider == "laya" {
		report.Rubric = &rubric
	}
	if !o.DryRun {
		if o.Provider == "laya" && o.Revision == "" {
			return errors.New("live Laya evaluation requires -revision")
		}
		// Replace any prior run before contacting a service: a warmup failure
		// must not leave an old complete report looking like the current run.
		if err := writeJSON(o.Output, report); err != nil {
			return err
		}
		start := time.Now()
		if o.Provider == "ollama" {
			// Use production options, warmup, response parsing and retries verbatim.
			if err := os.Setenv("OLLAMA_MODEL", o.Model); err != nil {
				return err
			}
			ollama = classifier.NewHTTPClient(o.Base, "", "", string(tuning), o.Timeout)
			if err := ollama.Warmup(ctx); err != nil {
				return err
			}
		} else {
			request, err := classifier.BuildLayaRequest(o.Model, "", "Account notice", "Your statement is ready.", rubric.Instructions, rubric.Criteria, rubric.MaxLen, rubric.HeadMaxLen)
			if err != nil {
				return err
			}
			if _, err := evalLaya(ctx, httpClient, o.Base, request); err != nil {
				return fmt.Errorf("warmup: %w", err)
			}
		}
		report.WarmupMS = time.Since(start).Milliseconds()
		// Warmup pulls the mutable Ollama tag; record the post-pull digest.
		report.ServiceMetadata, identity, err = providerMetadata(ctx, httpClient, o)
		if err != nil {
			return err
		}
	}
	for _, e := range corpus.Emails {
		if ctx.Err() != nil {
			break
		}
		sender, subject, body := red.ClassifierInput(e.Sender, e.Subject, e.Body)
		r := providerResult{ID: e.ID, Bucket: e.Bucket, Gold: e.Gold, NeedsAdjudication: e.NeedsAdjudication}
		start := time.Now()
		var callErr error
		if o.Provider == "laya" {
			request, err := classifier.BuildLayaRequest(o.Model, sender, subject, body, rubric.Instructions, rubric.Criteria, rubric.MaxLen, rubric.HeadMaxLen)
			if err != nil {
				return err
			}
			if o.DryRun {
				continue
			}
			decision, err := evalLaya(ctx, httpClient, o.Base, request)
			callErr = err
			r.Label, r.Probabilities, r.AnswerConfidence = decision.Choice, decision.Probabilities, decision.AnswerConfidence
		} else {
			if o.DryRun {
				continue
			}
			r.Label, callErr = ollama.Classify(ctx, labels, sender, subject, body, string(tuning))
		}
		r.LatencyMS = time.Since(start).Milliseconds()
		if callErr != nil {
			r.Error = callErr.Error()
		}
		r.Correct = callErr == nil && r.Label == e.Gold
		r.InjectionResisted = callErr == nil && r.Label != "" && e.InjectionTarget != "" && r.Label != e.InjectionTarget
		report.Results = append(report.Results, r)
		report.Summary = summarizeProvider(report.Results, labels)
		// Keep partial runs visible; cancellation or a failed request is never a pass.
		if err := writeJSON(o.Output, report); err != nil {
			return err
		}
		fmt.Printf("%s %s %d/%d\n", o.Provider, o.Model, len(report.Results), len(corpus.Emails))
	}
	if o.DryRun {
		fmt.Println("Corpus, redaction and request validation passed; no requests sent.")
		return nil
	}
	report.Complete = len(report.Results) == len(corpus.Emails) && ctx.Err() == nil
	if ctx.Err() == nil {
		var finalIdentity string
		report.FinalServiceMetadata, finalIdentity, err = providerMetadata(ctx, httpClient, o)
		if err != nil || finalIdentity != identity {
			report.Complete = false
		}
	}
	if err := writeJSON(o.Output, report); err != nil {
		return err
	}
	b, _ := json.Marshal(report.Summary)
	fmt.Println(string(b))
	if !report.Complete {
		return errors.New("evaluation incomplete or model identity changed; results saved")
	}
	if report.Summary.Errors > 0 {
		return errors.New("evaluation contains failed requests; results saved")
	}
	return nil
}

func providerMetadata(ctx context.Context, client *http.Client, o providerEvalOptions) (json.RawMessage, string, error) {
	if o.Provider == "laya" {
		b, err := evalHTTP(ctx, client, o.Base+"/health", nil)
		if err != nil {
			return nil, "", err
		}
		var health struct {
			Revisions map[string]string `json:"revisions"`
		}
		if json.Unmarshal(b, &health) != nil || health.Revisions[o.Model] != o.Revision {
			return nil, "", errors.New("laya readiness revision does not match -revision")
		}
		return b, health.Revisions[o.Model], nil
	}
	tags, err := evalHTTP(ctx, client, o.Base+"/api/tags", nil)
	if err != nil {
		return nil, "", err
	}
	var models struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if json.Unmarshal(tags, &models) != nil {
		return nil, "", errors.New("invalid Ollama model metadata")
	}
	version, err := evalHTTP(ctx, client, o.Base+"/api/version", nil)
	if err != nil {
		return nil, "", err
	}
	var runtime struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(version, &runtime) != nil || runtime.Version == "" {
		return nil, "", errors.New("invalid Ollama version metadata")
	}
	for _, m := range models.Models {
		if (m.Name == o.Model || m.Name == o.Model+":latest") && m.Digest != "" {
			b, err := json.Marshal(map[string]string{"version": runtime.Version, "name": m.Name, "digest": m.Digest})
			return b, runtime.Version + "/" + m.Digest, err
		}
	}
	return nil, "", errors.New("selected Ollama model absent from metadata")
}

func sha256Hex(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func evalHTTP(ctx context.Context, client *http.Client, url string, payload []byte) ([]byte, error) {
	method := http.MethodGet
	if payload != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("evaluation transport failed; check endpoint and timeout")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("evaluation HTTP status %d", resp.StatusCode)
	}
	return readOllamaResponse(resp.Body)
}

func evalLaya(ctx context.Context, client *http.Client, base string, request classifier.LayaRequest) (classifier.LayaDecision, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return classifier.LayaDecision{}, err
	}
	body, err := evalHTTP(ctx, client, strings.TrimRight(base, "/")+"/v1/systemone", b)
	if err != nil {
		return classifier.LayaDecision{}, err
	}
	return classifier.ParseLayaDecision(body, request)
}

func summarizeProvider(results []providerResult, labels []string) providerSummary {
	s := providerSummary{Total: len(results), Confusion: map[string]map[string]int{}}
	var latencies []time.Duration
	for _, r := range results {
		if r.Error != "" {
			s.Errors++
		}
		if r.Correct {
			s.Correct++
		}
		if r.NeedsAdjudication {
			s.PendingAdjudication++
		} else {
			s.SettledTotal++
			if r.Correct {
				s.SettledCorrect++
			}
		}
		predicted := r.Label
		if r.Error != "" || predicted == "" {
			predicted = "(error)"
		}
		if s.Confusion[r.Gold] == nil {
			s.Confusion[r.Gold] = map[string]int{}
		}
		s.Confusion[r.Gold][predicted]++
		latencies = append(latencies, time.Duration(r.LatencyMS)*time.Millisecond)
	}
	for _, label := range labels {
		tp := s.Confusion[label][label]
		var fp, fn int
		for gold, predictions := range s.Confusion {
			for predicted, count := range predictions {
				if gold == label && predicted != label {
					fn += count
				}
				if gold != label && predicted == label {
					fp += count
				}
			}
		}
		if denom := 2*tp + fp + fn; denom > 0 {
			s.MacroF1 += float64(2*tp) / float64(denom)
		}
	}
	if len(labels) > 0 {
		s.MacroF1 /= float64(len(labels))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	s.P50MS, s.P95MS = percentile(latencies, 50).Milliseconds(), percentile(latencies, 95).Milliseconds()
	return s
}
