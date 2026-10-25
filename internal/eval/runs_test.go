package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/genai"
)

func TestLoadRunBundle(t *testing.T) {
	corpus := validCorpus(t)
	bundle := validRunBundle(t, corpus, "development")
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadRunBundle(bytes.NewReader(raw), corpus)
	if err != nil {
		t.Fatalf("LoadRunBundle: %v", err)
	}
	if len(got.Runs) != 24 {
		t.Fatalf("loaded %d runs, want 24", len(got.Runs))
	}
}

func TestLoadRunBundleRejectsUnknownField(t *testing.T) {
	corpus := validCorpus(t)
	raw, err := json.Marshal(validRunBundle(t, corpus, "development"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"evaluationId":`), []byte(`"extra":"fake","evaluationId":`), 1)
	if _, err := LoadRunBundle(bytes.NewReader(raw), corpus); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestLoadRunBundleRejectsSecretBearingField(t *testing.T) {
	corpus := validCorpus(t)
	raw, err := json.Marshal(validRunBundle(t, corpus, "development"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"evaluationId":`), []byte(`"apiKey":"fake","evaluationId":`), 1)
	if _, err := LoadRunBundle(bytes.NewReader(raw), corpus); err == nil || !strings.Contains(err.Error(), "secret-bearing field") {
		t.Fatalf("secret field error = %v", err)
	}
}

func TestValidateRunBundleInvariants(t *testing.T) {
	tests := []struct {
		name string
		edit func(*RunBundle)
		want string
	}{
		{"coverage", func(b *RunBundle) { b.Runs = b.Runs[:len(b.Runs)-1] }, "complete paired runs"},
		{"duplicate slot", func(b *RunBundle) {
			b.Runs[1].ConfigurationID = b.Runs[0].ConfigurationID
			b.Runs[1].CaseID = b.Runs[0].CaseID
			b.Runs[1].Repetition = b.Runs[0].Repetition
		}, "slot"},
		{"prd binding", func(b *RunBundle) { b.Runs[0].PRDSHA256 = strings.Repeat("0", 64) }, "does not match case"},
		{"raw hash", func(b *RunBundle) { b.Runs[0].RawResponse += "changed" }, "referenced bytes"},
		{"parsed hash", func(b *RunBundle) { b.Runs[0].Drafts[0].Title = "changed" }, "referenced bytes"},
		{"success cycle", func(b *RunBundle) {
			b.Runs[0].Drafts[0].Dependencies = []int{2}
			b.Runs[0].ParsedDraftsSHA256 = canonicalDraftDigest(b.Runs[0].Drafts)
		}, "claims success for invalid drafts"},
		{"mapping missing", func(b *RunBundle) { b.Runs[0].Adjudication.TaskLabels = b.Runs[0].Adjudication.TaskLabels[:1] }, "task labels"},
		{"mapping reference", func(b *RunBundle) { b.Runs[0].Adjudication.TaskLabels[0].RequirementIDs = []string{"missing"} }, "unknown or duplicate"},
		{"edge label", func(b *RunBundle) { b.Runs[0].Adjudication.EdgeLabels = nil }, "edge labels"},
		{"blind", func(b *RunBundle) { b.Runs[0].Adjudication.Blind = false }, "blind must be true"},
		{"credential setting", func(b *RunBundle) { b.Configurations[0].GenerationSettings = "token=fake" }, "credential"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			corpus := validCorpus(t)
			bundle := cloneRunBundle(t, validRunBundle(t, corpus, "development"))
			tc.edit(bundle)
			err := ValidateRunBundle(bundle, corpus)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRunBundle error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateRunBundleAcceptsRecordedProviderFailure(t *testing.T) {
	corpus := validCorpus(t)
	bundle := validRunBundle(t, corpus, "development")
	run := &bundle.Runs[0]
	run.RawResponse = ""
	run.RawResponseSHA256 = hashBytes(nil)
	run.Drafts = []genai.Draft{}
	run.ParsedDraftsSHA256 = canonicalDraftDigest(run.Drafts)
	run.Outcome = "provider-error"
	run.StopClassification = "provider-error"
	run.Publication = PublicationObservation{Atomic: true}
	run.Adjudication = nil
	if err := ValidateRunBundle(bundle, corpus); err != nil {
		t.Fatalf("provider failure should remain a gradeable observation: %v", err)
	}
}

func TestValidateRunBundleDispositionOutcomeCoherence(t *testing.T) {
	tests := []struct {
		name                string
		expectedDisposition string
		outcome             string
		disposition         string
		wantErr             bool
	}{
		{name: "success with refuse", outcome: "success", disposition: "refuse", wantErr: true},
		{name: "refusal with generate", outcome: "refusal", disposition: "generate", wantErr: true},
		{name: "refusal with warn", outcome: "refusal", disposition: "warn", wantErr: true},
		{name: "refusal with refuse", outcome: "refusal", disposition: "refuse"},
		{name: "success with generate", outcome: "success", disposition: "generate"},
		{name: "success with warn", outcome: "success", disposition: "warn"},
		{name: "provider error with generate", outcome: "provider-error", disposition: "generate"},
		{name: "expected refuse but generated", expectedDisposition: "refuse", outcome: "success", disposition: "generate"},
		{name: "expected generate but refused", expectedDisposition: "generate", outcome: "refusal", disposition: "refuse"},
		{name: "expected warn but generated", expectedDisposition: "warn", outcome: "success", disposition: "generate"},
		{name: "expected generate but warned", expectedDisposition: "generate", outcome: "success", disposition: "warn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			corpus := validCorpus(t)
			bundle := validRunBundle(t, corpus, "development")
			run := &bundle.Runs[0]
			if tc.expectedDisposition != "" {
				for i := range corpus.Cases {
					if corpus.Cases[i].ID == run.CaseID {
						corpus.Cases[i].ExpectedDisposition = tc.expectedDisposition
					}
				}
			}
			setRunOutcome(run, tc.outcome, tc.disposition)
			err := ValidateRunBundle(bundle, corpus)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "conflicts with outcome") {
					t.Fatalf("ValidateRunBundle error = %v, want disposition/outcome conflict", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRunBundle: %v", err)
			}
		})
	}
}

func validRunBundle(t *testing.T, corpus *Corpus, split string) *RunBundle {
	t.Helper()
	promptHash := hashBytes([]byte("frozen prompt"))
	bundle := &RunBundle{
		Version: RunBundleVersion, EvaluationID: "pilot-2026-08-11",
		CorpusVersion: corpus.Version, RubricVersion: corpus.RubricVersion,
		Split: split, Repetitions: expectedRepetitions,
		Configurations: []Configuration{
			{ID: "baseline", Role: "baseline", ToolCommit: strings.Repeat("a", 40), ToolVersion: "baseline", PromptSHA256: promptHash, Provider: "fixture", RequestedModel: "model-a", ResolvedModel: "model-a-20260801", ModelPinned: true, Transport: "fixture", GenerationSettings: "requested-tasks=2"},
			{ID: "candidate", Role: "candidate", ToolCommit: strings.Repeat("b", 40), ToolVersion: "candidate", PromptSHA256: promptHash, Provider: "fixture", RequestedModel: "model-b", ResolvedModel: "model-b-20260801", ModelPinned: true, Transport: "fixture", GenerationSettings: "requested-tasks=2"},
		},
	}
	for _, config := range bundle.Configurations {
		for i := range corpus.Cases {
			caseDef := &corpus.Cases[i]
			if caseDef.Split != split {
				continue
			}
			for repetition := 1; repetition <= bundle.Repetitions; repetition++ {
				bundle.Runs = append(bundle.Runs, validRun(t, config.ID, caseDef, repetition))
			}
		}
	}
	return bundle
}

func validRun(t *testing.T, configID string, caseDef *Case, repetition int) Run {
	t.Helper()
	drafts := []genai.Draft{
		{Title: "Create storage", Description: "Create durable storage.", Details: "Verify restart behavior.", Priority: "high", Dependencies: []int{}},
		{Title: "Build service", Description: "Build the service on storage.", Details: "Verify service behavior.", Priority: "medium", Dependencies: []int{1}},
	}
	raw, err := json.Marshal(drafts)
	if err != nil {
		t.Fatal(err)
	}
	return Run{
		ID:              configID + "-" + caseDef.ID + "-" + string(rune('0'+repetition)),
		ConfigurationID: configID, CaseID: caseDef.ID, Repetition: repetition,
		PRDSHA256: caseDef.PRD.SHA256, RequestedTasks: 2,
		StartedAt: "2026-08-11T00:00:00Z", DurationMilliseconds: 100,
		RawResponse: string(raw), RawResponseSHA256: hashBytes(raw), ParsedDraftsSHA256: hashBytes(raw),
		Outcome: "success", StopClassification: "completed", Disposition: "generate", Drafts: drafts,
		Publication: PublicationObservation{Attempted: true, Atomic: true, PostRemapVerified: true},
		Adjudication: &Adjudication{
			Blind: true, Graders: []string{"grader-a", "grader-b"}, Adjudicator: "grader-c",
			TaskLabels: []TaskLabel{
				{TaskIndex: 1, RequirementIDs: []string{"storage"}, Grounded: true, Actionability: 1, AcceptanceQuality: .9, PriorityCalibration: 1},
				{TaskIndex: 2, RequirementIDs: []string{"service"}, Grounded: true, Actionability: 1, AcceptanceQuality: .9, PriorityCalibration: 1},
			},
			OrderingLabels: []OrderingLabel{{AxiomID: "storage-before-service", Satisfied: true}},
			EdgeLabels:     []EdgeLabel{{TaskIndex: 2, DependencyIndex: 1, Justified: true}},
			Usability:      "no-revision",
		},
	}
}

func cloneRunBundle(t *testing.T, bundle *RunBundle) *RunBundle {
	t.Helper()
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var clone RunBundle
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func setRunOutcome(run *Run, outcome, disposition string) {
	run.Outcome = outcome
	run.Disposition = disposition
	if outcome == "success" {
		return
	}
	run.RawResponse = ""
	run.RawResponseSHA256 = hashBytes(nil)
	run.Drafts = []genai.Draft{}
	run.ParsedDraftsSHA256 = canonicalDraftDigest(run.Drafts)
	run.StopClassification = outcome
	run.Publication = PublicationObservation{Atomic: true}
	run.Adjudication = nil
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func canonicalDraftDigest(drafts []genai.Draft) string {
	raw, _ := json.Marshal(drafts)
	return hashBytes(raw)
}
