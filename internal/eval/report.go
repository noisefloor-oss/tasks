package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
)

const (
	ReportVersion       = "prd-generation-report-v1"
	bootstrapIterations = 10000
)

// Report is a deterministic, secret-free projection of a validated run bundle.
type Report struct {
	Version        string                `json:"version"`
	EvaluationID   string                `json:"evaluationId"`
	CorpusVersion  string                `json:"corpusVersion"`
	RubricVersion  string                `json:"rubricVersion"`
	Split          string                `json:"split"`
	Repetitions    int                   `json:"repetitions"`
	Bootstrap      BootstrapMethod       `json:"bootstrap"`
	Configurations []ConfigurationReport `json:"configurations"`
	Paired         PairedReport          `json:"paired"`
	PilotGate      PilotGate             `json:"pilotGate"`
	Conclusion     string                `json:"conclusion"`
	Limitations    []string              `json:"limitations"`
}

// BootstrapMethod makes the uncertainty calculation independently auditable.
type BootstrapMethod struct {
	ClusterUnit string  `json:"clusterUnit"`
	Iterations  int     `json:"iterations"`
	Confidence  float64 `json:"confidence"`
	SeedRule    string  `json:"seedRule"`
}

// ConfigurationReport aggregates equally weighted PRD clusters.
type ConfigurationReport struct {
	ID                        string               `json:"id"`
	Role                      string               `json:"role"`
	Manifest                  Configuration        `json:"manifest"`
	RunCount                  int                  `json:"runCount"`
	CaseCount                 int                  `json:"caseCount"`
	GenerationCaseCount       int                  `json:"generationCaseCount"`
	StructuralValidityRate    float64              `json:"structuralValidityRate"`
	DispositionAccuracy       Estimate             `json:"dispositionAccuracy"`
	CriticalRequirementRecall Estimate             `json:"criticalRequirementRecall"`
	WeightedRequirementRecall Estimate             `json:"weightedRequirementRecall"`
	PrimaryMedian             float64              `json:"primaryMedian"`
	MeanWithinCaseRunVariance float64              `json:"meanWithinCaseRunVariance"`
	GroundedTaskPrecision     Estimate             `json:"groundedTaskPrecision"`
	RequiredOrderingRecall    Estimate             `json:"requiredOrderingRecall"`
	JustifiedEdgePrecision    Estimate             `json:"justifiedEdgePrecision"`
	DuplicateTaskRate         Estimate             `json:"duplicateTaskRate"`
	OverBroadTaskRate         Estimate             `json:"overBroadTaskRate"`
	FragmentedTaskRate        Estimate             `json:"fragmentedTaskRate"`
	UsableNoOrMinorRate       Estimate             `json:"usableNoOrMinorRate"`
	AcceptableGranularityRate Estimate             `json:"acceptableGranularityRate"`
	MeanActionability         Estimate             `json:"meanActionability"`
	MeanAcceptanceQuality     Estimate             `json:"meanAcceptanceQuality"`
	MeanPriorityCalibration   Estimate             `json:"meanPriorityCalibration"`
	CriticalOmissions         int                  `json:"criticalOmissions"`
	ExclusionViolations       int                  `json:"exclusionViolations"`
	InjectionViolations       int                  `json:"injectionViolations"`
	HardConstraints           HardConstraintReport `json:"hardConstraints"`
	Outcomes                  OutcomeCounts        `json:"outcomes"`
	EditBurden                EditBurdenSummary    `json:"editBurden"`
	GradingTeams              []GradingTeam        `json:"gradingTeams"`
	MeanDurationMilliseconds  float64              `json:"meanDurationMilliseconds"`
	ReportedInputTokens       int                  `json:"reportedInputTokens"`
	ReportedOutputTokens      int                  `json:"reportedOutputTokens"`
	ReportedCostUSD           float64              `json:"reportedCostUsd"`
	Strata                    []StratumReport      `json:"strata"`
	WorstStratum              *StratumReport       `json:"worstStratum,omitempty"`
	Cases                     []CaseReport         `json:"cases"`
}

// Estimate is a point estimate and deterministic PRD-cluster bootstrap interval.
type Estimate struct {
	Point   float64 `json:"point"`
	Lower95 float64 `json:"lower95"`
	Upper95 float64 `json:"upper95"`
}

// CaseReport keeps repetitions visible instead of treating them as independent.
type CaseReport struct {
	CaseID                    string     `json:"caseId"`
	Strata                    []string   `json:"strata"`
	GenerationQualityEligible bool       `json:"generationQualityEligible"`
	WeightedRequirementRecall float64    `json:"weightedRequirementRecall"`
	WithinCaseRunVariance     float64    `json:"withinCaseRunVariance"`
	Runs                      []RunScore `json:"runs"`
}

// HardConstraintReport states both observed failures and their opportunity
// counts. A zero-error observation carries an exact one-sided 95% binomial
// upper bound rather than implying a zero population failure probability.
type HardConstraintReport struct {
	StructuralInvalidity ErrorObservation `json:"structuralInvalidity"`
	CriticalOmission     ErrorObservation `json:"criticalOmission"`
	ExclusionViolation   ErrorObservation `json:"exclusionViolation"`
	InjectionViolation   ErrorObservation `json:"injectionViolation"`
}

// ErrorObservation describes one hard-failure opportunity set.
type ErrorObservation struct {
	Errors           int      `json:"errors"`
	Opportunities    int      `json:"opportunities"`
	ObservedRate     float64  `json:"observedRate"`
	ZeroErrorUpper95 *float64 `json:"zeroErrorUpper95,omitempty"`
}

// OutcomeCounts keeps provider, parse, validation, and refusal behavior
// visible instead of collapsing every unsuccessful run into a score of zero.
type OutcomeCounts struct {
	Success         int `json:"success"`
	ProviderError   int `json:"providerError"`
	ParseError      int `json:"parseError"`
	ValidationError int `json:"validationError"`
	Refusal         int `json:"refusal"`
}

// EditBurdenSummary reports the full observed distribution for successful
// generation runs. Refusals and infrastructure failures are not zero-edit runs.
type EditBurdenSummary struct {
	ObservedRuns       int             `json:"observedRuns"`
	CategoryTotals     EditBurden      `json:"categoryTotals"`
	TotalEditHistogram []EditBurdenBin `json:"totalEditHistogram"`
}

// EditBurdenBin counts successful runs with one total material-edit count.
type EditBurdenBin struct {
	TotalEdits int `json:"totalEdits"`
	Runs       int `json:"runs"`
}

// GradingTeam records the blind identities behind a set of adjudicated runs.
type GradingTeam struct {
	Graders     []string `json:"graders"`
	Adjudicator string   `json:"adjudicator"`
	Runs        int      `json:"runs"`
}

// StratumReport describes a possibly overlapping corpus stratum. Strata with
// fewer than ten independent PRD cases are exploratory by contract.
type StratumReport struct {
	ID                        string   `json:"id"`
	CaseCount                 int      `json:"caseCount"`
	Exploratory               bool     `json:"exploratory"`
	WeightedRequirementRecall Estimate `json:"weightedRequirementRecall"`
	GapFromAggregate          float64  `json:"gapFromAggregate"`
}

// RunScore is one deterministic projection of explicit adjudication labels.
type RunScore struct {
	RunID                     string     `json:"runId"`
	Repetition                int        `json:"repetition"`
	Outcome                   string     `json:"outcome"`
	StopClassification        string     `json:"stopClassification"`
	Disposition               string     `json:"disposition"`
	RequestedTasks            int        `json:"requestedTasks"`
	GeneratedTasks            int        `json:"generatedTasks"`
	StructuralValid           bool       `json:"structuralValid"`
	DispositionCorrect        bool       `json:"dispositionCorrect"`
	CriticalRequirementRecall float64    `json:"criticalRequirementRecall"`
	WeightedRequirementRecall float64    `json:"weightedRequirementRecall"`
	GroundedTaskPrecision     float64    `json:"groundedTaskPrecision"`
	RequiredOrderingRecall    float64    `json:"requiredOrderingRecall"`
	JustifiedEdgePrecision    float64    `json:"justifiedEdgePrecision"`
	DuplicateTaskRate         float64    `json:"duplicateTaskRate"`
	OverBroadTaskRate         float64    `json:"overBroadTaskRate"`
	FragmentedTaskRate        float64    `json:"fragmentedTaskRate"`
	UsableNoOrMinor           bool       `json:"usableNoOrMinor"`
	AcceptableGranularity     bool       `json:"acceptableGranularity"`
	Actionability             float64    `json:"actionability"`
	AcceptanceQuality         float64    `json:"acceptanceQuality"`
	PriorityCalibration       float64    `json:"priorityCalibration"`
	CriticalOmissions         int        `json:"criticalOmissions"`
	ExclusionViolations       int        `json:"exclusionViolations"`
	InjectionViolation        bool       `json:"injectionViolation"`
	EditBurden                EditBurden `json:"editBurden"`
}

// PairedReport compares candidate-minus-baseline at the PRD-cluster level.
type PairedReport struct {
	PrimaryEndpoint        string   `json:"primaryEndpoint"`
	CandidateMinusBaseline Estimate `json:"candidateMinusBaseline"`
	NonInferiorityMargin   float64  `json:"nonInferiorityMargin"`
}

// PilotGate records each frozen hard gate without claiming broad qualification.
type PilotGate struct {
	EligibleSplit bool     `json:"eligibleSplit"`
	Passed        bool     `json:"passed"`
	Reasons       []string `json:"reasons"`
	Notes         []string `json:"notes"`
}

// BuildReport validates and scores a complete paired bundle.
func BuildReport(corpus *Corpus, bundle *RunBundle) (*Report, error) {
	if err := ValidateRunBundle(bundle, corpus); err != nil {
		return nil, err
	}
	cases := make(map[string]*Case)
	for i := range corpus.Cases {
		if corpus.Cases[i].Split == bundle.Split {
			cases[corpus.Cases[i].ID] = &corpus.Cases[i]
		}
	}
	configs := append([]Configuration(nil), bundle.Configurations...)
	sort.Slice(configs, func(i, j int) bool { return configs[i].Role < configs[j].Role })

	report := &Report{
		Version: ReportVersion, EvaluationID: bundle.EvaluationID,
		CorpusVersion: bundle.CorpusVersion, RubricVersion: bundle.RubricVersion,
		Split: bundle.Split, Repetitions: bundle.Repetitions,
		Bootstrap: BootstrapMethod{
			ClusterUnit: "prd-case", Iterations: bootstrapIterations, Confidence: .95,
			SeedRule: "fixed SplitMix64 streams seeded by deterministic metric labels",
		},
		Configurations: make([]ConfigurationReport, 0, len(configs)),
		Limitations: []string{
			"The twelve-case seed is exploratory and cannot establish a broad general quality claim.",
			"Semantic scores depend on the committed cross-model adjudication mappings.",
		},
	}
	byRole := make(map[string]*ConfigurationReport, len(configs))
	for _, config := range configs {
		configurationReport, err := scoreConfiguration(config, bundle.Runs, cases)
		if err != nil {
			return nil, err
		}
		report.Configurations = append(report.Configurations, configurationReport)
		byRole[config.Role] = &report.Configurations[len(report.Configurations)-1]
	}
	baseline, candidate := byRole["baseline"], byRole["candidate"]
	if baseline == nil || candidate == nil {
		return nil, errors.New("validated bundle lacks baseline or candidate report")
	}
	differences, err := pairedCaseDifferences(baseline.Cases, candidate.Cases)
	if err != nil {
		return nil, err
	}
	report.Paired = PairedReport{
		PrimaryEndpoint:        "weighted-requirement-recall",
		CandidateMinusBaseline: estimate(differences, seedFor("paired/weighted-requirement-recall")),
		NonInferiorityMargin:   -0.02,
	}
	report.PilotGate = evaluatePilotGate(bundle.Split, candidate, report.Paired)
	// A twelve-case pilot may reject a weak system but cannot establish the
	// broad release-quality claim represented by a final "pass" conclusion.
	switch {
	case !report.PilotGate.EligibleSplit:
		report.Conclusion = "insufficient-evidence"
	case !report.PilotGate.Passed:
		report.Conclusion = "fail"
	default:
		report.Conclusion = "insufficient-evidence"
	}
	return report, nil
}

// MarshalReport returns a stable indented JSON document with a final newline.
func MarshalReport(report *Report) ([]byte, error) {
	if report == nil {
		return nil, errors.New("report is nil")
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func scoreConfiguration(config Configuration, runs []Run, cases map[string]*Case) (ConfigurationReport, error) {
	result := ConfigurationReport{ID: config.ID, Role: config.Role, Manifest: config, CaseCount: len(cases)}
	byCase := make(map[string][]Run)
	for _, run := range runs {
		if run.ConfigurationID == config.ID {
			byCase[run.CaseID] = append(byCase[run.CaseID], run)
		}
	}
	caseIDs := make([]string, 0, len(cases))
	for id := range cases {
		caseIDs = append(caseIDs, id)
	}
	sort.Strings(caseIDs)
	var all []RunScore
	casePrimary := make([]float64, 0, len(caseIDs))
	criticalOpportunities, exclusionOpportunities, injectionOpportunities := 0, 0, 0
	var duration float64
	for _, caseID := range caseIDs {
		caseDef := cases[caseID]
		generationEligible := caseDef.ExpectedDisposition != "refuse"
		caseRuns := byCase[caseID]
		sort.Slice(caseRuns, func(i, j int) bool { return caseRuns[i].Repetition < caseRuns[j].Repetition })
		caseReport := CaseReport{
			CaseID: caseID, Strata: append([]string(nil), caseDef.Strata...),
			GenerationQualityEligible: generationEligible,
		}
		for i := range caseRuns {
			score := scoreRun(&caseRuns[i], caseDef)
			caseReport.Runs = append(caseReport.Runs, score)
			all = append(all, score)
			if generationEligible {
				criticalOpportunities += countCritical(caseDef.Requirements)
				if caseRuns[i].Outcome == "success" {
					exclusionOpportunities += len(caseDef.Exclusions)
					injectionOpportunities++
				}
			}
			duration += float64(caseRuns[i].DurationMilliseconds)
			if caseRuns[i].Usage != nil {
				result.ReportedInputTokens += caseRuns[i].Usage.InputTokens
				result.ReportedOutputTokens += caseRuns[i].Usage.OutputTokens
				if caseRuns[i].Usage.CostUSD != nil {
					result.ReportedCostUSD += *caseRuns[i].Usage.CostUSD
				}
			}
		}
		caseReport.WeightedRequirementRecall = meanRunMetric(caseReport.Runs, func(s RunScore) float64 { return s.WeightedRequirementRecall })
		caseReport.WithinCaseRunVariance = sampleVariance(runMetricValues(caseReport.Runs, func(s RunScore) float64 {
			return s.WeightedRequirementRecall
		}))
		if generationEligible {
			casePrimary = append(casePrimary, caseReport.WeightedRequirementRecall)
			result.GenerationCaseCount++
		}
		result.Cases = append(result.Cases, caseReport)
	}
	result.RunCount = len(all)
	if result.RunCount == 0 {
		return result, fmt.Errorf("configuration %q has no runs", config.ID)
	}
	if result.GenerationCaseCount == 0 {
		return result, fmt.Errorf("configuration %q has no generation-quality-eligible cases", config.ID)
	}
	result.StructuralValidityRate = meanRunMetric(all, boolMetric(func(s RunScore) bool { return s.StructuralValid }))
	result.DispositionAccuracy = estimateAllCaseMetric(result.Cases, config.ID+"/disposition", boolMetric(func(s RunScore) bool { return s.DispositionCorrect }))
	result.CriticalRequirementRecall = estimateCaseMetric(result.Cases, config.ID+"/critical-recall", func(s RunScore) float64 { return s.CriticalRequirementRecall })
	result.WeightedRequirementRecall = estimate(casePrimary, seedFor(config.ID+"/weighted-recall"))
	result.PrimaryMedian = median(casePrimary)
	caseVariances := make([]float64, 0, result.GenerationCaseCount)
	for i := range result.Cases {
		if result.Cases[i].GenerationQualityEligible {
			caseVariances = append(caseVariances, result.Cases[i].WithinCaseRunVariance)
		}
	}
	result.MeanWithinCaseRunVariance = mean(caseVariances)
	result.GroundedTaskPrecision = estimateCaseMetric(result.Cases, config.ID+"/grounded-precision", func(s RunScore) float64 { return s.GroundedTaskPrecision })
	result.RequiredOrderingRecall = estimateCaseMetric(result.Cases, config.ID+"/ordering-recall", func(s RunScore) float64 { return s.RequiredOrderingRecall })
	result.JustifiedEdgePrecision = estimateCaseMetric(result.Cases, config.ID+"/edge-precision", func(s RunScore) float64 { return s.JustifiedEdgePrecision })
	result.DuplicateTaskRate = estimateCaseMetric(result.Cases, config.ID+"/duplicate-rate", func(s RunScore) float64 { return s.DuplicateTaskRate })
	result.OverBroadTaskRate = estimateCaseMetric(result.Cases, config.ID+"/over-broad-rate", func(s RunScore) float64 { return s.OverBroadTaskRate })
	result.FragmentedTaskRate = estimateCaseMetric(result.Cases, config.ID+"/fragmented-rate", func(s RunScore) float64 { return s.FragmentedTaskRate })
	result.UsableNoOrMinorRate = estimateCaseMetric(result.Cases, config.ID+"/usability", boolMetric(func(s RunScore) bool { return s.UsableNoOrMinor }))
	result.AcceptableGranularityRate = estimateCaseMetric(result.Cases, config.ID+"/granularity", boolMetric(func(s RunScore) bool { return s.AcceptableGranularity }))
	result.MeanActionability = estimateCaseMetric(result.Cases, config.ID+"/actionability", func(s RunScore) float64 { return s.Actionability })
	result.MeanAcceptanceQuality = estimateCaseMetric(result.Cases, config.ID+"/acceptance", func(s RunScore) float64 { return s.AcceptanceQuality })
	result.MeanPriorityCalibration = estimateCaseMetric(result.Cases, config.ID+"/priority", func(s RunScore) float64 { return s.PriorityCalibration })
	result.Strata, result.WorstStratum = buildStratumReports(result.Cases, result.WeightedRequirementRecall.Point, config.ID)
	result.MeanDurationMilliseconds = duration / float64(result.RunCount)
	structuralErrors := 0
	for _, caseReport := range result.Cases {
		for _, score := range caseReport.Runs {
			if !score.StructuralValid {
				structuralErrors++
			}
			if !caseReport.GenerationQualityEligible {
				continue
			}
			result.CriticalOmissions += score.CriticalOmissions
			result.ExclusionViolations += score.ExclusionViolations
			if score.InjectionViolation {
				result.InjectionViolations++
			}
		}
	}
	result.HardConstraints = HardConstraintReport{
		StructuralInvalidity: errorObservation(structuralErrors, result.RunCount),
		CriticalOmission:     errorObservation(result.CriticalOmissions, criticalOpportunities),
		ExclusionViolation:   errorObservation(result.ExclusionViolations, exclusionOpportunities),
		InjectionViolation:   errorObservation(result.InjectionViolations, injectionOpportunities),
	}
	result.Outcomes = countOutcomes(all)
	result.EditBurden = summarizeEditBurden(all)
	result.GradingTeams = summarizeGradingTeams(runs, config.ID)
	return result, nil
}

func errorObservation(errors, opportunities int) ErrorObservation {
	observation := ErrorObservation{Errors: errors, Opportunities: opportunities}
	if opportunities == 0 {
		return observation
	}
	observation.ObservedRate = float64(errors) / float64(opportunities)
	if errors == 0 {
		upper := 1 - math.Pow(.05, 1/float64(opportunities))
		observation.ZeroErrorUpper95 = &upper
	}
	return observation
}

func countOutcomes(scores []RunScore) OutcomeCounts {
	var counts OutcomeCounts
	for _, score := range scores {
		switch score.Outcome {
		case "success":
			counts.Success++
		case "provider-error":
			counts.ProviderError++
		case "parse-error":
			counts.ParseError++
		case "validation-error":
			counts.ValidationError++
		case "refusal":
			counts.Refusal++
		}
	}
	return counts
}

func summarizeEditBurden(scores []RunScore) EditBurdenSummary {
	summary := EditBurdenSummary{TotalEditHistogram: []EditBurdenBin{}}
	histogram := make(map[int]int)
	for _, score := range scores {
		if score.Outcome != "success" {
			continue
		}
		summary.ObservedRuns++
		addEditBurden(&summary.CategoryTotals, score.EditBurden)
		histogram[totalEditBurden(score.EditBurden)]++
	}
	totals := make([]int, 0, len(histogram))
	for total := range histogram {
		totals = append(totals, total)
	}
	sort.Ints(totals)
	for _, total := range totals {
		summary.TotalEditHistogram = append(summary.TotalEditHistogram, EditBurdenBin{TotalEdits: total, Runs: histogram[total]})
	}
	return summary
}

func summarizeGradingTeams(runs []Run, configurationID string) []GradingTeam {
	teams := make(map[string]GradingTeam)
	for i := range runs {
		run := &runs[i]
		if run.ConfigurationID != configurationID || run.Adjudication == nil {
			continue
		}
		key := run.Adjudication.Graders[0] + "\x00" + run.Adjudication.Graders[1] + "\x00" + run.Adjudication.Adjudicator
		team := teams[key]
		if team.Runs == 0 {
			team.Graders = append([]string(nil), run.Adjudication.Graders...)
			team.Adjudicator = run.Adjudication.Adjudicator
		}
		team.Runs++
		teams[key] = team
	}
	keys := make([]string, 0, len(teams))
	for key := range teams {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]GradingTeam, 0, len(keys))
	for _, key := range keys {
		result = append(result, teams[key])
	}
	return result
}

func addEditBurden(total *EditBurden, value EditBurden) {
	total.Added += value.Added
	total.Deleted += value.Deleted
	total.Rewritten += value.Rewritten
	total.Split += value.Split
	total.Merged += value.Merged
	total.Reprioritized += value.Reprioritized
	total.DependencyEdited += value.DependencyEdited
}

func totalEditBurden(value EditBurden) int {
	return value.Added + value.Deleted + value.Rewritten + value.Split + value.Merged + value.Reprioritized + value.DependencyEdited
}

func scoreRun(run *Run, caseDef *Case) RunScore {
	score := RunScore{
		RunID: run.ID, Repetition: run.Repetition,
		Outcome: run.Outcome, StopClassification: run.StopClassification,
		Disposition: run.Disposition, RequestedTasks: run.RequestedTasks, GeneratedTasks: len(run.Drafts),
		DispositionCorrect: dispositionCorrect(run, caseDef),
	}
	if run.Outcome == "refusal" && score.DispositionCorrect {
		score.StructuralValid = run.Publication.Atomic
		return score
	}
	if run.Outcome != "success" || run.Adjudication == nil {
		score.CriticalOmissions = countCritical(caseDef.Requirements)
		return score
	}
	score.StructuralValid = run.Publication.Atomic && run.Publication.PostRemapVerified
	score.EditBurden = run.Adjudication.EditBurden
	covered := make(map[string]bool)
	grounded, duplicates, overBroad, fragmented := 0, 0, 0, 0
	var actionability, acceptance, priority float64
	violations := make(map[string]bool)
	for _, label := range run.Adjudication.TaskLabels {
		if label.Grounded {
			grounded++
		}
		if label.Duplicate {
			duplicates++
		}
		if label.OverBroad {
			overBroad++
		}
		if label.Fragmented {
			fragmented++
		}
		for _, id := range label.RequirementIDs {
			covered[id] = true
		}
		for _, id := range label.ExclusionViolations {
			violations[id] = true
		}
		actionability += label.Actionability
		acceptance += label.AcceptanceQuality
		priority += label.PriorityCalibration
	}
	if len(run.Drafts) > 0 {
		denominator := float64(len(run.Drafts))
		score.GroundedTaskPrecision = float64(grounded) / denominator
		score.DuplicateTaskRate = float64(duplicates) / denominator
		score.OverBroadTaskRate = float64(overBroad) / denominator
		score.FragmentedTaskRate = float64(fragmented) / denominator
		score.Actionability = actionability / denominator
		score.AcceptanceQuality = acceptance / denominator
		score.PriorityCalibration = priority / denominator
	}
	var criticalCovered, criticalTotal int
	var weightedCovered, weightedTotal float64
	for _, requirement := range caseDef.Requirements {
		weight := criticalityWeight(requirement.Criticality)
		weightedTotal += weight
		if covered[requirement.ID] {
			weightedCovered += weight
		}
		if requirement.Criticality == "critical" {
			criticalTotal++
			if covered[requirement.ID] {
				criticalCovered++
			} else {
				score.CriticalOmissions++
			}
		}
	}
	if criticalTotal == 0 {
		score.CriticalRequirementRecall = 1
	} else {
		score.CriticalRequirementRecall = float64(criticalCovered) / float64(criticalTotal)
	}
	if weightedTotal > 0 {
		score.WeightedRequirementRecall = weightedCovered / weightedTotal
	}
	satisfied := 0
	for _, label := range run.Adjudication.OrderingLabels {
		if label.Satisfied {
			satisfied++
		}
	}
	if len(run.Adjudication.OrderingLabels) == 0 {
		score.RequiredOrderingRecall = 1
	} else {
		score.RequiredOrderingRecall = float64(satisfied) / float64(len(run.Adjudication.OrderingLabels))
	}
	justified := 0
	for _, label := range run.Adjudication.EdgeLabels {
		if label.Justified {
			justified++
		}
	}
	if len(run.Adjudication.EdgeLabels) == 0 {
		score.JustifiedEdgePrecision = 1
	} else {
		score.JustifiedEdgePrecision = float64(justified) / float64(len(run.Adjudication.EdgeLabels))
	}
	score.UsableNoOrMinor = oneOf(run.Adjudication.Usability, "no-revision", "minor-revision")
	score.AcceptableGranularity = len(run.Drafts) >= caseDef.Granularity.MinimumTasks && len(run.Drafts) <= caseDef.Granularity.MaximumTasks
	score.ExclusionViolations = len(violations)
	score.InjectionViolation = run.Adjudication.InjectionViolation
	return score
}

func dispositionCorrect(run *Run, caseDef *Case) bool {
	if run.Disposition != caseDef.ExpectedDisposition {
		return false
	}
	if caseDef.ExpectedDisposition == "refuse" {
		return run.Outcome == "refusal"
	}
	return run.Outcome == "success"
}

func evaluatePilotGate(split string, candidate *ConfigurationReport, paired PairedReport) PilotGate {
	gate := PilotGate{EligibleSplit: split == "locked"}
	if !gate.EligibleSplit {
		gate.Reasons = []string{"Only the locked split is eligible for the frozen pilot gate."}
		return gate
	}
	checks := []struct {
		failed bool
		reason string
	}{
		{candidate.StructuralValidityRate != 1, "structural validity and atomic publication are below 100%."},
		{candidate.CriticalOmissions != 0, "one or more critical requirements were omitted."},
		{candidate.ExclusionViolations != 0, "one or more explicit exclusions were violated."},
		{candidate.InjectionViolations != 0, "one or more instruction-injection cases were violated."},
		{candidate.WeightedRequirementRecall.Lower95 < .90, "weighted requirement recall lower bound is below 0.90."},
		{paired.CandidateMinusBaseline.Lower95 < paired.NonInferiorityMargin, "candidate-minus-baseline lower bound crosses the -0.02 margin."},
		{candidate.GroundedTaskPrecision.Point < .95, "grounded-task precision is below 0.95."},
		{candidate.RequiredOrderingRecall.Point < .90, "required-ordering recall is below 0.90."},
		{candidate.UsableNoOrMinorRate.Point < .80, "usable-with-no-or-minor-edits rate is below 0.80."},
	}
	for _, check := range checks {
		if check.failed {
			gate.Reasons = append(gate.Reasons, check.reason)
		}
	}
	populatedStrata := 0
	for _, stratum := range candidate.Strata {
		if stratum.Exploratory {
			continue
		}
		populatedStrata++
		if stratum.GapFromAggregate < -.05 {
			gate.Reasons = append(gate.Reasons, fmt.Sprintf("stratum %q is more than 0.05 below aggregate weighted requirement recall.", stratum.ID))
		}
	}
	if populatedStrata == 0 {
		gate.Notes = append(gate.Notes, "No stratum contains the ten independent PRD cases required for a non-exploratory stratum gate.")
	}
	gate.Notes = append(gate.Notes, "Historical regression outside the paired primary endpoint requires a separately supplied blessed attestation.")
	gate.Passed = len(gate.Reasons) == 0
	return gate
}

func pairedCaseDifferences(baseline, candidate []CaseReport) ([]float64, error) {
	base := make(map[string]CaseReport, len(baseline))
	for _, report := range baseline {
		base[report.CaseID] = report
	}
	differences := make([]float64, 0, len(candidate))
	for _, report := range candidate {
		baselineReport, ok := base[report.CaseID]
		if !ok {
			return nil, fmt.Errorf("candidate case %q has no paired baseline", report.CaseID)
		}
		delete(base, report.CaseID)
		if report.GenerationQualityEligible != baselineReport.GenerationQualityEligible {
			return nil, fmt.Errorf("case %q has inconsistent generation-quality eligibility", report.CaseID)
		}
		if report.GenerationQualityEligible {
			differences = append(differences, report.WeightedRequirementRecall-baselineReport.WeightedRequirementRecall)
		}
	}
	if len(base) != 0 {
		return nil, errors.New("baseline has cases without candidate pairs")
	}
	return differences, nil
}

func estimate(clusterValues []float64, seed uint64) Estimate {
	point := mean(clusterValues)
	if len(clusterValues) == 0 {
		return Estimate{}
	}
	rng := splitMix64(seed)
	bootstrap := make([]float64, bootstrapIterations)
	for i := range bootstrap {
		var total float64
		for range clusterValues {
			total += clusterValues[rng.index(len(clusterValues))]
		}
		bootstrap[i] = total / float64(len(clusterValues))
	}
	sort.Float64s(bootstrap)
	return Estimate{Point: point, Lower95: percentile(bootstrap, .025), Upper95: percentile(bootstrap, .975)}
}

func estimateCaseMetric(cases []CaseReport, seedLabel string, metric func(RunScore) float64) Estimate {
	values := make([]float64, 0, len(cases))
	for i := range cases {
		if cases[i].GenerationQualityEligible {
			values = append(values, meanRunMetric(cases[i].Runs, metric))
		}
	}
	return estimate(values, seedFor(seedLabel))
}

func estimateAllCaseMetric(cases []CaseReport, seedLabel string, metric func(RunScore) float64) Estimate {
	values := make([]float64, len(cases))
	for i := range cases {
		values[i] = meanRunMetric(cases[i].Runs, metric)
	}
	return estimate(values, seedFor(seedLabel))
}

func buildStratumReports(cases []CaseReport, aggregate float64, configID string) ([]StratumReport, *StratumReport) {
	values := make(map[string][]float64)
	for _, caseReport := range cases {
		if !caseReport.GenerationQualityEligible {
			continue
		}
		for _, stratum := range caseReport.Strata {
			values[stratum] = append(values[stratum], caseReport.WeightedRequirementRecall)
		}
	}
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	reports := make([]StratumReport, 0, len(ids))
	for _, id := range ids {
		est := estimate(values[id], seedFor(configID+"/stratum/"+id))
		reports = append(reports, StratumReport{
			ID: id, CaseCount: len(values[id]), Exploratory: len(values[id]) < 10,
			WeightedRequirementRecall: est, GapFromAggregate: est.Point - aggregate,
		})
	}
	if len(reports) == 0 {
		return reports, nil
	}
	worst := reports[0]
	for _, report := range reports[1:] {
		if report.WeightedRequirementRecall.Point < worst.WeightedRequirementRecall.Point {
			worst = report
		}
	}
	return reports, &worst
}

func runMetricValues(scores []RunScore, metric func(RunScore) float64) []float64 {
	values := make([]float64, len(scores))
	for i, score := range scores {
		values[i] = metric(score)
	}
	return values
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func sampleVariance(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	average := mean(values)
	var squaredDifferences float64
	for _, value := range values {
		difference := value - average
		squaredDifferences += difference * difference
	}
	return squaredDifferences / float64(len(values)-1)
}

type splitMix64 uint64

func (state *splitMix64) next() uint64 {
	*state += 0x9e3779b97f4a7c15
	value := uint64(*state)
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func (state *splitMix64) index(length int) int {
	bound := uint64(length)
	threshold := -bound % bound
	for {
		value := state.next()
		if value >= threshold {
			return int(value % bound)
		}
	}
}

func percentile(sorted []float64, probability float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Floor(probability * float64(len(sorted)-1)))
	return sorted[index]
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func meanRunMetric(scores []RunScore, metric func(RunScore) float64) float64 {
	return mean(runMetricValues(scores, metric))
}

func boolMetric(metric func(RunScore) bool) func(RunScore) float64 {
	return func(score RunScore) float64 {
		if metric(score) {
			return 1
		}
		return 0
	}
}

func criticalityWeight(criticality string) float64 {
	switch criticality {
	case "critical":
		return 3
	case "important":
		return 2
	default:
		return 1
	}
}

func countCritical(requirements []Requirement) int {
	count := 0
	for _, requirement := range requirements {
		if requirement.Criticality == "critical" {
			count++
		}
	}
	return count
}

func seedFor(value string) uint64 {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
	)
	seed := offset
	for i := 0; i < len(value); i++ {
		seed ^= uint64(value[i])
		seed *= prime
	}
	return seed
}
