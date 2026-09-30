package classifier

import (
	"errors"
	"strings"
	"testing"
)

func TestLayaDecisionBoundary(t *testing.T) {
	request, err := BuildLayaRequest("english", "sender", "subject", "body", "Choose", map[string]string{"Primary": "Human correspondence", "Updates": "Automated notices"}, 1024, 384)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"routing":{"model":"english"},"answers":{"category":{"type":"choice","choice":"Primary","probabilities":{"Primary":0.8,"Updates":0.2},"confidence":0.05,"answer_confidence":0.8}}}`
	decision, err := ParseLayaDecision([]byte(valid), request)
	if err != nil || decision.Choice != "Primary" || decision.AnswerConfidence != 0.8 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	for name, body := range map[string]string{
		"missing":                   `{}`,
		"wrong model":               strings.Replace(valid, `"english"`, `"multilingual"`, 1),
		"unknown label":             strings.Replace(valid, `"choice":"Primary"`, `"choice":"secret email body"`, 1),
		"nonwinning label":          strings.Replace(valid, `"choice":"Primary"`, `"choice":"Updates"`, 1),
		"missing probability":       strings.Replace(valid, `,"Updates":0.2`, ``, 1),
		"null probability":          strings.Replace(valid, `"Updates":0.2`, `"Updates":null`, 1),
		"bad total":                 strings.Replace(valid, `"Updates":0.2`, `"Updates":0.3`, 1),
		"no probability confidence": strings.Replace(valid, `,"answer_confidence":0.8`, ``, 1),
		"collapsed":                 strings.Replace(valid, `"routing":`, `"usage":{"options":{"category":{"total":2,"distinct":1}}},"routing":`, 1),
		"invalid json":              `{"error":"secret email body"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseLayaDecision([]byte(body), request)
			var retired *NoAllowedLabelError
			if err == nil || errors.As(err, &retired) || strings.Contains(err.Error(), "secret email body") {
				t.Fatalf("unsafe acceptance/error: %v", err)
			}
		})
	}
	request.Model = "auto"
	if _, err := BuildLayaRequest(request.Model, "", "", "", "Choose", request.Questions["category"].Criteria, 1024, 384); err == nil {
		t.Fatal("accepted auto-routing")
	}
}
