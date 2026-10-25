package eval

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/genai"
)

const expectedRepetitions = 3

// LoadRunBundle decodes a closed run bundle and verifies its bindings to a
// previously validated corpus.
func LoadRunBundle(r io.Reader, corpus *Corpus) (*RunBundle, error) {
	raw, err := readJSONDocument(r, maximumRunBundleBytes, "run bundle")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var bundle RunBundle
	if err := dec.Decode(&bundle); err != nil {
		return nil, fmt.Errorf("decode run bundle: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode run bundle: trailing JSON value")
		}
		return nil, fmt.Errorf("decode run bundle trailing data: %w", err)
	}
	if err := ValidateRunBundle(&bundle, corpus); err != nil {
		return nil, err
	}
	return &bundle, nil
}

// ValidateRunBundle validates manifests and complete paired coverage. Model
// quality failures remain valid observations; inconsistent manifests do not.
func ValidateRunBundle(bundle *RunBundle, corpus *Corpus) error {
	if bundle == nil {
		return errors.New("run bundle is nil")
	}
	if err := ValidateCorpus(corpus); err != nil {
		return fmt.Errorf("validate bound corpus: %w", err)
	}
	if bundle.Version != RunBundleVersion {
		return fmt.Errorf("run bundle version %q, want %q", bundle.Version, RunBundleVersion)
	}
	if !stableID.MatchString(bundle.EvaluationID) {
		return fmt.Errorf("evaluationId %q is invalid", bundle.EvaluationID)
	}
	if bundle.CorpusVersion != corpus.Version || bundle.RubricVersion != corpus.RubricVersion {
		return errors.New("run bundle corpus or rubric version does not match the corpus")
	}
	if !oneOf(bundle.Split, "calibration", "development", "locked") {
		return fmt.Errorf("run bundle split %q is invalid", bundle.Split)
	}
	if bundle.Repetitions != expectedRepetitions {
		return fmt.Errorf("run bundle repetitions is %d, want %d", bundle.Repetitions, expectedRepetitions)
	}

	configs, err := validateConfigurations(bundle.Configurations)
	if err != nil {
		return err
	}
	cases := make(map[string]*Case)
	for i := range corpus.Cases {
		if corpus.Cases[i].Split == bundle.Split {
			cases[corpus.Cases[i].ID] = &corpus.Cases[i]
		}
	}
	wantRuns := len(configs) * len(cases) * bundle.Repetitions
	if len(bundle.Runs) != wantRuns {
		return fmt.Errorf("run bundle has %d runs, want %d complete paired runs", len(bundle.Runs), wantRuns)
	}

	seenID := make(map[string]bool, len(bundle.Runs))
	seenSlot := make(map[string]bool, len(bundle.Runs))
	for i := range bundle.Runs {
		run := &bundle.Runs[i]
		path := fmt.Sprintf("runs[%d]", i)
		if !stableID.MatchString(run.ID) || seenID[run.ID] {
			return fmt.Errorf("%s.id %q is invalid or duplicated", path, run.ID)
		}
		seenID[run.ID] = true
		config := configs[run.ConfigurationID]
		if config == nil {
			return fmt.Errorf("%s.configurationId %q is unknown", path, run.ConfigurationID)
		}
		caseDef := cases[run.CaseID]
		if caseDef == nil {
			return fmt.Errorf("%s.caseId %q is not in split %q", path, run.CaseID, bundle.Split)
		}
		if run.Repetition < 1 || run.Repetition > bundle.Repetitions {
			return fmt.Errorf("%s.repetition %d is outside [1,%d]", path, run.Repetition, bundle.Repetitions)
		}
		slot := run.ConfigurationID + "\x00" + run.CaseID + "\x00" + strconv.Itoa(run.Repetition)
		if seenSlot[slot] {
			return fmt.Errorf("%s duplicates configuration/case/repetition slot", path)
		}
		seenSlot[slot] = true
		if err := validateRun(run, config, caseDef, path); err != nil {
			return err
		}
	}
	return nil
}

func validateConfigurations(configurations []Configuration) (map[string]*Configuration, error) {
	if len(configurations) != 2 {
		return nil, fmt.Errorf("run bundle has %d configurations, want baseline and candidate", len(configurations))
	}
	byID := make(map[string]*Configuration, len(configurations))
	roles := make(map[string]bool, len(configurations))
	for i := range configurations {
		config := &configurations[i]
		path := fmt.Sprintf("configurations[%d]", i)
		if !stableID.MatchString(config.ID) || byID[config.ID] != nil {
			return nil, fmt.Errorf("%s.id %q is invalid or duplicated", path, config.ID)
		}
		if !oneOf(config.Role, "baseline", "candidate") || roles[config.Role] {
			return nil, fmt.Errorf("%s.role %q is invalid or duplicated", path, config.Role)
		}
		roles[config.Role] = true
		if !hexLength(config.ToolCommit, 40, 64) {
			return nil, fmt.Errorf("%s.toolCommit is not a 40- or 64-character hex digest", path)
		}
		if err := validateDigest(config.PromptSHA256, path+".promptSha256"); err != nil {
			return nil, err
		}
		fields := []struct {
			name, value string
		}{
			{"toolVersion", config.ToolVersion},
			{"provider", config.Provider},
			{"requestedModel", config.RequestedModel},
			{"resolvedModel", config.ResolvedModel},
			{"transport", config.Transport},
			{"generationSettings", config.GenerationSettings},
		}
		for _, field := range fields {
			if strings.TrimSpace(field.value) == "" {
				return nil, fmt.Errorf("%s.%s is empty", path, field.name)
			}
		}
		if containsCredentialAssignment(config.GenerationSettings) {
			return nil, fmt.Errorf("%s.generationSettings appears to contain a credential", path)
		}
		if !config.ModelPinned {
			return nil, fmt.Errorf("%s.modelPinned must be true for an evaluation bundle", path)
		}
		byID[config.ID] = config
	}
	if !roles["baseline"] || !roles["candidate"] {
		return nil, errors.New("run bundle requires one baseline and one candidate configuration")
	}
	return byID, nil
}

func validateRun(run *Run, _ *Configuration, caseDef *Case, path string) error {
	if run.PRDSHA256 != caseDef.PRD.SHA256 {
		return fmt.Errorf("%s.prdSha256 does not match case %q", path, caseDef.ID)
	}
	if run.RequestedTasks < 1 || run.RequestedTasks > pilotMaximumTasks {
		return fmt.Errorf("%s.requestedTasks %d is invalid", path, run.RequestedTasks)
	}
	if _, err := time.Parse(time.RFC3339Nano, run.StartedAt); err != nil {
		return fmt.Errorf("%s.startedAt is not RFC3339: %w", path, err)
	}
	if run.DurationMilliseconds < 0 {
		return fmt.Errorf("%s.durationMilliseconds is negative", path)
	}
	if err := validateUsage(run.Usage, path+".usage"); err != nil {
		return err
	}
	if err := requireDigest(run.RawResponseSHA256, []byte(run.RawResponse), path+".rawResponseSha256"); err != nil {
		return err
	}
	if run.Drafts == nil {
		return fmt.Errorf("%s.drafts must be present (use an empty array)", path)
	}
	parsed, err := json.Marshal(run.Drafts)
	if err != nil {
		return fmt.Errorf("%s.drafts: %w", path, err)
	}
	if err := requireDigest(run.ParsedDraftsSHA256, parsed, path+".parsedDraftsSha256"); err != nil {
		return err
	}
	if !oneOf(run.Outcome, "success", "provider-error", "parse-error", "validation-error", "refusal") {
		return fmt.Errorf("%s.outcome %q is invalid", path, run.Outcome)
	}
	if !oneOf(run.StopClassification, "completed", "provider-error", "parse-error", "validation-error", "refusal", "truncated") {
		return fmt.Errorf("%s.stopClassification %q is invalid", path, run.StopClassification)
	}
	if !oneOf(run.Disposition, "generate", "warn", "refuse") {
		return fmt.Errorf("%s.disposition %q is invalid", path, run.Disposition)
	}
	if (run.Disposition == "refuse") != (run.Outcome == "refusal") {
		return fmt.Errorf("%s disposition %q conflicts with outcome %q", path, run.Disposition, run.Outcome)
	}
	draftErr := validateGeneratedDrafts(run.Drafts, run.RequestedTasks)
	if run.Outcome == "success" {
		if draftErr != nil {
			return fmt.Errorf("%s claims success for invalid drafts: %w", path, draftErr)
		}
		if run.StopClassification != "completed" || !run.Publication.Attempted {
			return fmt.Errorf("%s successful run lacks a completed generation and publication attempt", path)
		}
		if run.Adjudication == nil {
			return fmt.Errorf("%s successful run lacks adjudication", path)
		}
		return validateAdjudication(run.Adjudication, run.Drafts, caseDef, path+".adjudication")
	}
	if run.Publication.Attempted || run.Publication.PostRemapVerified {
		return fmt.Errorf("%s unsuccessful run claims publication", path)
	}
	if run.Adjudication != nil {
		return fmt.Errorf("%s unsuccessful run must not carry semantic adjudication", path)
	}
	if run.Outcome == "validation-error" && draftErr == nil {
		return fmt.Errorf("%s claims validation-error for structurally valid drafts", path)
	}
	if run.Outcome != "validation-error" && len(run.Drafts) != 0 {
		return fmt.Errorf("%s non-validation failure must carry an empty drafts array", path)
	}
	wantStop := map[string]string{
		"provider-error":   "provider-error",
		"parse-error":      "parse-error",
		"validation-error": "validation-error",
		"refusal":          "refusal",
	}[run.Outcome]
	if run.StopClassification != wantStop && !(run.Outcome == "parse-error" && run.StopClassification == "truncated") {
		return fmt.Errorf("%s outcome %q conflicts with stopClassification %q", path, run.Outcome, run.StopClassification)
	}
	return nil
}

func validateUsage(usage *Usage, path string) error {
	if usage == nil {
		return nil
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return fmt.Errorf("%s token count is negative", path)
	}
	if usage.CostUSD != nil && (math.IsNaN(*usage.CostUSD) || math.IsInf(*usage.CostUSD, 0) || *usage.CostUSD < 0) {
		return fmt.Errorf("%s.costUsd is invalid", path)
	}
	return nil
}

func validateGeneratedDrafts(drafts []genai.Draft, requested int) error {
	if len(drafts) > requested {
		return fmt.Errorf("produced %d drafts above requested cap %d", len(drafts), requested)
	}
	if err := genai.ValidateDrafts(drafts); err != nil {
		return err
	}
	nodes := make(map[string]bool, len(drafts))
	edges := make(map[string][]string, len(drafts))
	for i, draft := range drafts {
		id := strconv.Itoa(i + 1)
		nodes[id] = true
		if strings.ContainsAny(draft.Title, "\r\n") {
			return fmt.Errorf("draft %d title contains a line break", i+1)
		}
		seen := make(map[int]bool, len(draft.Dependencies))
		for _, dependency := range draft.Dependencies {
			if seen[dependency] {
				return fmt.Errorf("draft %d repeats dependency %d", i+1, dependency)
			}
			seen[dependency] = true
			edges[id] = append(edges[id], strconv.Itoa(dependency))
		}
	}
	if cycle := firstCycle(nodes, edges); len(cycle) > 0 {
		return fmt.Errorf("draft dependencies contain a cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

func validateAdjudication(a *Adjudication, drafts []genai.Draft, caseDef *Case, path string) error {
	if !a.Blind {
		return fmt.Errorf("%s.blind must be true", path)
	}
	if len(a.Graders) != 2 || !normalizedIdentity(a.Graders[0]) || !normalizedIdentity(a.Graders[1]) || a.Graders[0] == a.Graders[1] {
		return fmt.Errorf("%s.graders must name two distinct blind graders", path)
	}
	if !normalizedIdentity(a.Adjudicator) || a.Adjudicator == a.Graders[0] || a.Adjudicator == a.Graders[1] {
		return fmt.Errorf("%s.adjudicator must be distinct from both graders", path)
	}
	requirements := make(map[string]bool, len(caseDef.Requirements))
	for _, requirement := range caseDef.Requirements {
		requirements[requirement.ID] = true
	}
	exclusions := make(map[string]bool, len(caseDef.Exclusions))
	for _, exclusion := range caseDef.Exclusions {
		exclusions[exclusion.ID] = true
	}
	if len(a.TaskLabels) != len(drafts) {
		return fmt.Errorf("%s has %d task labels, want %d", path, len(a.TaskLabels), len(drafts))
	}
	seenTasks := make(map[int]bool, len(drafts))
	for i, label := range a.TaskLabels {
		p := fmt.Sprintf("%s.taskLabels[%d]", path, i)
		if label.TaskIndex < 1 || label.TaskIndex > len(drafts) || seenTasks[label.TaskIndex] {
			return fmt.Errorf("%s.taskIndex %d is invalid or duplicated", p, label.TaskIndex)
		}
		seenTasks[label.TaskIndex] = true
		if label.Grounded != (len(label.RequirementIDs) > 0) {
			return fmt.Errorf("%s.grounded must equal whether requirementIds is nonempty", p)
		}
		if err := validateStringReferences(label.RequirementIDs, requirements, p+".requirementIds"); err != nil {
			return err
		}
		if err := validateStringReferences(label.ExclusionViolations, exclusions, p+".exclusionViolations"); err != nil {
			return err
		}
		scores := []struct {
			name  string
			value float64
		}{
			{"actionability", label.Actionability},
			{"acceptanceQuality", label.AcceptanceQuality},
			{"priorityCalibration", label.PriorityCalibration},
		}
		for _, score := range scores {
			if math.IsNaN(score.value) || math.IsInf(score.value, 0) || score.value < 0 || score.value > 1 {
				return fmt.Errorf("%s.%s is outside [0,1]", p, score.name)
			}
		}
	}
	if err := validateOrderingLabels(a.OrderingLabels, caseDef, path); err != nil {
		return err
	}
	if err := validateEdgeLabels(a.EdgeLabels, drafts, path); err != nil {
		return err
	}
	if !oneOf(a.Usability, "no-revision", "minor-revision", "major-revision", "complete-revision") {
		return fmt.Errorf("%s.usability %q is invalid", path, a.Usability)
	}
	if err := validateEditBurden(a.EditBurden, path+".editBurden"); err != nil {
		return err
	}
	edits := a.EditBurden.Added + a.EditBurden.Deleted + a.EditBurden.Rewritten + a.EditBurden.Split + a.EditBurden.Merged + a.EditBurden.Reprioritized + a.EditBurden.DependencyEdited
	if a.Usability == "no-revision" && edits != 0 {
		return fmt.Errorf("%s no-revision usability conflicts with nonzero edit burden", path)
	}
	if a.Usability != "no-revision" && edits == 0 {
		return fmt.Errorf("%s revision usability requires a nonzero edit burden", path)
	}
	return nil
}

func validateStringReferences(values []string, allowed map[string]bool, path string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !allowed[value] || seen[value] {
			return fmt.Errorf("%s contains unknown or duplicate reference %q", path, value)
		}
		seen[value] = true
	}
	return nil
}

func validateOrderingLabels(labels []OrderingLabel, caseDef *Case, path string) error {
	want := make(map[string]bool, len(caseDef.OrderingAxioms))
	for _, axiom := range caseDef.OrderingAxioms {
		want[axiom.ID] = true
	}
	if len(labels) != len(want) {
		return fmt.Errorf("%s has %d ordering labels, want %d", path, len(labels), len(want))
	}
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		if !want[label.AxiomID] || seen[label.AxiomID] {
			return fmt.Errorf("%s has unknown or duplicate ordering label %q", path, label.AxiomID)
		}
		seen[label.AxiomID] = true
	}
	return nil
}

func validateEdgeLabels(labels []EdgeLabel, drafts []genai.Draft, path string) error {
	want := make(map[string]bool)
	for i, draft := range drafts {
		for _, dependency := range draft.Dependencies {
			want[edgeKey(i+1, dependency)] = true
		}
	}
	if len(labels) != len(want) {
		return fmt.Errorf("%s has %d edge labels, want %d", path, len(labels), len(want))
	}
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		key := edgeKey(label.TaskIndex, label.DependencyIndex)
		if !want[key] || seen[key] {
			return fmt.Errorf("%s has unknown or duplicate edge label %q", path, key)
		}
		seen[key] = true
	}
	return nil
}

func validateEditBurden(b EditBurden, path string) error {
	values := []int{b.Added, b.Deleted, b.Rewritten, b.Split, b.Merged, b.Reprioritized, b.DependencyEdited}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("%s contains a negative count", path)
		}
	}
	return nil
}

func edgeKey(task, dependency int) string {
	return strconv.Itoa(task) + "<-" + strconv.Itoa(dependency)
}

func containsCredentialAssignment(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"api_key=", "apikey=", "password=", "private_key=", "secret=", "token="} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hexLength(value string, lengths ...int) bool {
	decoded, err := hex.DecodeString(value)
	if err != nil || value != strings.ToLower(value) {
		return false
	}
	for _, length := range lengths {
		if len(decoded)*2 == length {
			return true
		}
	}
	return false
}
