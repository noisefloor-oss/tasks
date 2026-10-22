package eval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/genai"
)

func TestBuildReportPerfectLockedPilot(t *testing.T) {
	corpus := validCorpus(t)
	bundle := validRunBundle(t, corpus, "locked")
	report, err := BuildReport(corpus, bundle)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(report.Configurations) != 2 {
		t.Fatalf("configurations = %d, want 2", len(report.Configurations))
	}
	candidate := configurationByRole(t, report, "candidate")
	if candidate.WeightedRequirementRecall.Point != 1 || candidate.WeightedRequirementRecall.Lower95 != 1 {
		t.Fatalf("candidate weighted recall = %#v, want all 1", candidate.WeightedRequirementRecall)
	}
	if candidate.Manifest.Provider != "fixture" || candidate.Manifest.ResolvedModel != "model-b-20260801" {
		t.Fatalf("candidate manifest = %#v", candidate.Manifest)
	}
	if candidate.Outcomes.Success != 12 || candidate.EditBurden.ObservedRuns != 12 || len(candidate.EditBurden.TotalEditHistogram) != 1 || candidate.EditBurden.TotalEditHistogram[0] != (EditBurdenBin{TotalEdits: 0, Runs: 12}) {
		t.Fatalf("candidate outcomes/edit burden = %#v / %#v", candidate.Outcomes, candidate.EditBurden)
	}
	if len(candidate.GradingTeams) != 1 || candidate.GradingTeams[0].Runs != 12 {
		t.Fatalf("candidate grading teams = %#v", candidate.GradingTeams)
	}
	if candidate.PrimaryMedian != 1 || candidate.MeanWithinCaseRunVariance != 0 {
		t.Fatalf("candidate primary median/variance = %v/%v", candidate.PrimaryMedian, candidate.MeanWithinCaseRunVariance)
	}
	if candidate.GroundedTaskPrecision.Point != 1 || candidate.DuplicateTaskRate.Point != 0 || candidate.OverBroadTaskRate.Point != 0 || candidate.FragmentedTaskRate.Point != 0 {
		t.Fatalf("candidate task quality rates = grounded %#v duplicate %#v over-broad %#v fragmented %#v", candidate.GroundedTaskPrecision, candidate.DuplicateTaskRate, candidate.OverBroadTaskRate, candidate.FragmentedTaskRate)
	}
	if len(candidate.Strata) == 0 || candidate.WorstStratum == nil || !candidate.WorstStratum.Exploratory {
		t.Fatalf("candidate stratum summaries = %#v / %#v", candidate.Strata, candidate.WorstStratum)
	}
	for name, observation := range map[string]ErrorObservation{
		"structural": candidate.HardConstraints.StructuralInvalidity,
		"critical":   candidate.HardConstraints.CriticalOmission,
		"exclusion":  candidate.HardConstraints.ExclusionViolation,
		"injection":  candidate.HardConstraints.InjectionViolation,
	} {
		if observation.Errors != 0 || observation.Opportunities != 12 || observation.ZeroErrorUpper95 == nil || *observation.ZeroErrorUpper95 <= 0 {
			t.Errorf("%s zero-error observation = %#v", name, observation)
		}
	}
	if !report.PilotGate.Passed || report.Conclusion != "insufficient-evidence" {
		t.Fatalf("gate/conclusion = %#v / %q", report.PilotGate, report.Conclusion)
	}
	if !containsReason(report.PilotGate.Notes, "ten independent PRD cases") {
		t.Fatalf("gate notes = %#v", report.PilotGate.Notes)
	}
	if report.Paired.CandidateMinusBaseline != (Estimate{Point: 0, Lower95: 0, Upper95: 0}) {
		t.Fatalf("paired estimate = %#v", report.Paired.CandidateMinusBaseline)
	}
}

func TestBuildReportHandlesExpectedRefusalOutsideGenerationMetrics(t *testing.T) {
	corpus := validCorpus(t)
	corpus.Cases[4].ExpectedDisposition = "refuse"
	bundle := validRunBundle(t, corpus, "development")
	for i := range bundle.Runs {
		run := &bundle.Runs[i]
		if run.CaseID != "development-1" {
			continue
		}
		run.RawResponse = ""
		run.RawResponseSHA256 = hashBytes(nil)
		run.Drafts = []genai.Draft{}
		run.ParsedDraftsSHA256 = canonicalDraftDigest(run.Drafts)
		run.Outcome = "refusal"
		run.StopClassification = "refusal"
		run.Disposition = "refuse"
		run.Publication = PublicationObservation{Atomic: true}
		run.Adjudication = nil
	}
	report, err := BuildReport(corpus, bundle)
	if err != nil {
		t.Fatal(err)
	}
	candidate := configurationByRole(t, report, "candidate")
	if candidate.GenerationCaseCount != 3 || candidate.StructuralValidityRate != 1 || candidate.DispositionAccuracy.Point != 1 || candidate.CriticalOmissions != 0 {
		t.Fatalf("refusal accounting = %#v", candidate)
	}
	if candidate.Outcomes != (OutcomeCounts{Success: 9, Refusal: 3}) {
		t.Fatalf("refusal outcomes = %#v", candidate.Outcomes)
	}
	if candidate.HardConstraints.CriticalOmission.Opportunities != 9 {
		t.Fatalf("critical opportunities = %#v, want 9", candidate.HardConstraints.CriticalOmission)
	}
	for _, caseReport := range candidate.Cases {
		if caseReport.CaseID == "development-1" && caseReport.GenerationQualityEligible {
			t.Fatalf("refusal case is generation-quality eligible: %#v", caseReport)
		}
	}
}

func TestBuildReportSurfacesTaskQualityLabels(t *testing.T) {
	corpus := validCorpus(t)
	bundle := validRunBundle(t, corpus, "development")
	for i := range bundle.Runs {
		if bundle.Runs[i].ConfigurationID != "candidate" {
			continue
		}
		bundle.Runs[i].Adjudication.TaskLabels[0].Duplicate = true
		bundle.Runs[i].Adjudication.TaskLabels[0].OverBroad = true
		bundle.Runs[i].Adjudication.TaskLabels[0].Fragmented = true
	}
	report, err := BuildReport(corpus, bundle)
	if err != nil {
		t.Fatal(err)
	}
	candidate := configurationByRole(t, report, "candidate")
	for name, estimate := range map[string]Estimate{
		"duplicate":  candidate.DuplicateTaskRate,
		"over-broad": candidate.OverBroadTaskRate,
		"fragmented": candidate.FragmentedTaskRate,
	} {
		if estimate.Point != .5 || estimate.Lower95 != .5 || estimate.Upper95 != .5 {
			t.Errorf("%s rate = %#v, want fixed 0.5", name, estimate)
		}
	}
}

func TestEvaluatePilotGateRejectsUnderperformingPopulatedStratum(t *testing.T) {
	perfect := Estimate{Point: 1, Lower95: 1, Upper95: 1}
	candidate := &ConfigurationReport{
		StructuralValidityRate:    1,
		WeightedRequirementRecall: perfect,
		GroundedTaskPrecision:     perfect,
		RequiredOrderingRecall:    perfect,
		UsableNoOrMinorRate:       perfect,
		Strata: []StratumReport{{
			ID: "security", CaseCount: 10,
			WeightedRequirementRecall: Estimate{Point: .94, Lower95: .9, Upper95: .98},
			GapFromAggregate:          -.06,
		}},
	}
	paired := PairedReport{CandidateMinusBaseline: Estimate{Lower95: 0}, NonInferiorityMargin: -.02}
	gate := evaluatePilotGate("locked", candidate, paired)
	if gate.Passed || !containsReason(gate.Reasons, `stratum "security"`) {
		t.Fatalf("gate = %#v", gate)
	}
}

func TestBuildReportFailsFrozenGateOnCandidateOmission(t *testing.T) {
	corpus := validCorpus(t)
	bundle := validRunBundle(t, corpus, "locked")
	for i := range bundle.Runs {
		run := &bundle.Runs[i]
		if run.ConfigurationID != "candidate" || run.CaseID != "locked-1" {
			continue
		}
		run.Adjudication.TaskLabels[1].RequirementIDs = []string{"storage"}
	}
	report, err := BuildReport(corpus, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if report.PilotGate.Passed || report.Conclusion != "fail" {
		t.Fatalf("gate/conclusion = %#v / %q", report.PilotGate, report.Conclusion)
	}
	if report.Paired.CandidateMinusBaseline.Point >= 0 {
		t.Fatalf("paired regression point = %v, want negative", report.Paired.CandidateMinusBaseline.Point)
	}
	if !containsReason(report.PilotGate.Reasons, "weighted requirement recall") {
		t.Fatalf("gate reasons = %#v", report.PilotGate.Reasons)
	}
}

func TestBuildReportCountsProviderFailureAsQualityFailure(t *testing.T) {
	corpus := validCorpus(t)
	bundle := validRunBundle(t, corpus, "development")
	run := &bundle.Runs[0]
	run.RawResponse = ""
	run.RawResponseSHA256 = hashBytes(nil)
	run.Drafts = run.Drafts[:0]
	run.ParsedDraftsSHA256 = canonicalDraftDigest(run.Drafts)
	run.Outcome = "provider-error"
	run.StopClassification = "provider-error"
	run.Publication = PublicationObservation{Atomic: true}
	run.Adjudication = nil
	report, err := BuildReport(corpus, bundle)
	if err != nil {
		t.Fatal(err)
	}
	baseline := configurationByRole(t, report, "baseline")
	if baseline.StructuralValidityRate >= 1 || baseline.WeightedRequirementRecall.Point >= 1 {
		t.Fatalf("provider failure was hidden: %#v", baseline)
	}
	if baseline.CriticalOmissions == 0 {
		t.Fatal("provider failure should count all critical requirements as omitted")
	}
	if baseline.Outcomes.ProviderError != 1 || baseline.Outcomes.Success != 11 {
		t.Fatalf("provider failure outcomes = %#v", baseline.Outcomes)
	}
	if baseline.HardConstraints.CriticalOmission.Opportunities != 12 || baseline.HardConstraints.ExclusionViolation.Opportunities != 11 || baseline.HardConstraints.InjectionViolation.Opportunities != 11 {
		t.Fatalf("provider failure opportunities = %#v", baseline.HardConstraints)
	}
}

func TestMarshalReportDeterministic(t *testing.T) {
	corpus := validCorpus(t)
	report, err := BuildReport(corpus, validRunBundle(t, corpus, "development"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := MarshalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("report JSON is not byte deterministic")
	}
	if first[len(first)-1] != '\n' {
		t.Fatal("report JSON lacks final newline")
	}
	var decoded Report
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	const goldenSHA256 = "6d965aec236890bbd126039a288261f0cc6012fe15013b4df900fba07c043111"
	if got := hashBytes(first); got != goldenSHA256 {
		t.Fatalf("report golden digest = %s; replace test constant after intentional schema changes", got)
	}
}

func configurationByRole(t *testing.T, report *Report, role string) ConfigurationReport {
	t.Helper()
	for _, configuration := range report.Configurations {
		if configuration.Role == role {
			return configuration
		}
	}
	t.Fatalf("role %q not found", role)
	return ConfigurationReport{}
}

func containsReason(reasons []string, fragment string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, fragment) {
			return true
		}
	}
	return false
}
