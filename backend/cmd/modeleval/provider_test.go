package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/redaction"
)

func TestProviderEvaluationSendsProductionRedaction(t *testing.T) {
	red, err := redaction.New(config.Default().Redaction.Patterns)
	if err != nil {
		t.Fatal(err)
	}
	sender, subject, body := red.ClassifierInput("person@example.com", "SSN 123-45-6789", strings.Repeat("界", 2100))
	r, err := classifier.BuildLayaRequest("english", sender, subject, body, "Choose", map[string]string{"Primary": "Person", "Updates": "Automated"}, 1024, 384)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got classifier.LayaRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/systemone" || strings.Contains(got.State["sender"], "example.com") || strings.Contains(got.State["subject"], "123-45") || len([]rune(got.State["body"])) != 2000 {
			t.Error("preprocessing or endpoint changed")
		}
		_, _ = w.Write([]byte(`{"routing":{"model":"english"},"answers":{"category":{"type":"choice","choice":"Primary","probabilities":{"Primary":0.8,"Updates":0.2},"confidence":0.1,"answer_confidence":0.8}}}`))
	}))
	defer s.Close()
	if _, err := evalLaya(context.Background(), s.Client(), s.URL, r); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSummaryCountsFailuresAndPendingLabels(t *testing.T) {
	s := summarizeProvider([]providerResult{
		{Gold: "Primary", Label: "Primary", Correct: true, LatencyMS: 1},
		{Gold: "Primary", Error: "timeout", LatencyMS: 100},
		{Gold: "Updates", Label: "Primary", NeedsAdjudication: true, LatencyMS: 2},
	}, []string{"Primary", "Updates"})
	if s.Total != 3 || s.Correct != 1 || s.Errors != 1 || s.SettledTotal != 2 || s.SettledCorrect != 1 || s.PendingAdjudication != 1 || math.Abs(s.MacroF1-0.25) > 1e-9 || s.P95MS != 100 {
		t.Fatalf("failures escaped denominator: %+v", s)
	}
}

func TestProviderReportPinsPostPullModelAndRejectsChangedIdentity(t *testing.T) {
	for _, changeAtEnd := range []bool{false, true} {
		t.Run(fmt.Sprint(changeAtEnd), func(t *testing.T) {
			t.Setenv("OLLAMA_MODEL", "")
			dir := t.TempDir()
			corpusPath, tuningPath, output := filepath.Join(dir, "corpus.json"), filepath.Join(dir, "TUNING.md"), filepath.Join(dir, "result.json")
			if err := os.WriteFile(corpusPath, []byte(`{"emails":[{"id":"one","bucket":"core","gold":"Primary","body":"hello"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tuningPath, []byte("## Allowed Labels\n- Primary\n- Updates\n"), 0600); err != nil {
				t.Fatal(err)
			}
			digest, reads := "before-pull", 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/pull":
					digest = "after-pull"
					_, _ = w.Write([]byte(`{}`))
				case "/api/generate":
					_, _ = w.Write([]byte(`{"response":"Primary"}`))
				case "/api/version":
					_, _ = w.Write([]byte(`{"version":"test-version"}`))
				case "/api/tags":
					reads++
					if changeAtEnd && reads == 2 {
						digest = "changed-during-run"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"name": "test:1", "digest": digest}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			err := runProviderEvaluation(context.Background(), providerEvalOptions{Provider: "ollama", Model: "test:1", Base: s.URL, CorpusPath: corpusPath, TuningPath: tuningPath, Output: output, Timeout: time.Second})
			if (err != nil) != changeAtEnd {
				t.Fatalf("run error: %v", err)
			}
			b, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Complete bool `json:"complete"`
				Metadata struct {
					Digest string `json:"digest"`
				} `json:"service_metadata"`
			}
			if err := json.Unmarshal(b, &report); err != nil {
				t.Fatal(err)
			}
			if report.Metadata.Digest != "after-pull" || report.Complete == changeAtEnd {
				t.Fatalf("wrong identity/complete: %+v", report)
			}
		})
	}
}
