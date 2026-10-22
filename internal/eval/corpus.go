package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
)

var stableID = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var forbiddenJSONKeys = map[string]bool{
	"apikey": true, "authorization": true, "credential": true,
	"environmentvalue": true, "password": true, "privatekey": true,
	"refreshtoken": true, "secrettoken": true,
}

// LoadCorpus decodes the closed corpus schema and validates every invariant.
func LoadCorpus(r io.Reader) (*Corpus, error) {
	raw, err := readJSONDocument(r, maximumCorpusBytes, "corpus")
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var corpus Corpus
	if err := dec.Decode(&corpus); err != nil {
		return nil, fmt.Errorf("decode corpus: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode corpus: trailing JSON value")
		}
		return nil, fmt.Errorf("decode corpus trailing data: %w", err)
	}
	if err := ValidateCorpus(&corpus); err != nil {
		return nil, err
	}
	return &corpus, nil
}

// ValidateCorpus checks the immutable twelve-case pilot contract.
func ValidateCorpus(c *Corpus) error {
	if c == nil {
		return errors.New("corpus is nil")
	}
	if c.Version != CorpusVersion {
		return fmt.Errorf("corpus version %q, want %q", c.Version, CorpusVersion)
	}
	if c.RubricVersion != RubricVersion {
		return fmt.Errorf("rubric version %q, want %q", c.RubricVersion, RubricVersion)
	}
	if strings.TrimSpace(c.AnnotationGuideVersion) == "" {
		return errors.New("annotationGuideVersion is empty")
	}
	if len(c.Cases) != pilotCaseCount {
		return fmt.Errorf("pilot corpus has %d cases, want exactly %d", len(c.Cases), pilotCaseCount)
	}

	byID := make(map[string]*Case, len(c.Cases))
	splits := map[string]int{"calibration": 0, "development": 0, "locked": 0}
	for i := range c.Cases {
		cs := &c.Cases[i]
		path := fmt.Sprintf("cases[%d]", i)
		if err := validateCase(cs, c.AnnotationGuideVersion, path); err != nil {
			return err
		}
		if _, exists := byID[cs.ID]; exists {
			return fmt.Errorf("%s.id %q is duplicated", path, cs.ID)
		}
		byID[cs.ID] = cs
		splits[cs.Split]++
	}
	for _, split := range []string{"calibration", "development", "locked"} {
		if splits[split] != pilotCasesPerSplit {
			return fmt.Errorf("split %q has %d cases, want %d", split, splits[split], pilotCasesPerSplit)
		}
	}
	baseEdges := make(map[string][]string, len(c.Cases))
	for i := range c.Cases {
		cs := &c.Cases[i]
		if cs.BaseCaseID == "" {
			continue
		}
		base := byID[cs.BaseCaseID]
		if base == nil {
			return fmt.Errorf("cases[%d].baseCaseId %q does not exist", i, cs.BaseCaseID)
		}
		if base.ID == cs.ID {
			return fmt.Errorf("cases[%d].baseCaseId references itself", i)
		}
		if base.Split != cs.Split {
			return fmt.Errorf("cases[%d] and base case %q are in different splits", i, base.ID)
		}
		if cs.Provenance.SourceSHA256 != base.PRD.SHA256 {
			return fmt.Errorf("cases[%d].provenance.sourceSha256 does not match base case %q PRD", i, base.ID)
		}
		baseEdges[cs.ID] = append(baseEdges[cs.ID], base.ID)
	}
	caseNodes := make(map[string]bool, len(byID))
	for id := range byID {
		caseNodes[id] = true
	}
	if cycle := firstCycle(caseNodes, baseEdges); len(cycle) > 0 {
		return fmt.Errorf("baseCaseId references contain a cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

func validateCase(c *Case, guideVersion, path string) error {
	if !stableID.MatchString(c.ID) {
		return fmt.Errorf("%s.id %q is not a stable lowercase id", path, c.ID)
	}
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("%s.version is empty", path)
	}
	if !oneOf(c.Split, "calibration", "development", "locked") {
		return fmt.Errorf("%s.split %q is invalid", path, c.Split)
	}
	if !oneOf(c.ExpectedDisposition, "generate", "warn", "refuse") {
		return fmt.Errorf("%s.expectedDisposition %q is invalid", path, c.ExpectedDisposition)
	}
	if err := validateProvenance(c, path); err != nil {
		return err
	}
	if err := validateStrata(c.Strata, path+".strata"); err != nil {
		return err
	}
	if strings.TrimSpace(c.PRD.Text) == "" {
		return fmt.Errorf("%s.prd.text is empty", path)
	}
	if err := requireDigest(c.PRD.SHA256, []byte(c.PRD.Text), path+".prd.sha256"); err != nil {
		return err
	}
	if len(c.Requirements) == 0 {
		return fmt.Errorf("%s.requirements is empty", path)
	}
	requirements, err := validateRequirements(c, path)
	if err != nil {
		return err
	}
	if err := validateOrderings(c, requirements, path); err != nil {
		return err
	}
	if err := validateExclusions(c, path); err != nil {
		return err
	}
	if err := validateGranularity(c.Granularity, path+".granularity"); err != nil {
		return err
	}
	return validateAnnotation(c.Annotation, guideVersion, c.Split, path+".annotation")
}

func validateProvenance(c *Case, path string) error {
	p := c.Provenance
	if !oneOf(p.Classification, "internal", "synthetic", "derived") {
		return fmt.Errorf("%s.provenance.classification %q is invalid", path, p.Classification)
	}
	fields := []struct {
		name  string
		value string
	}{
		{"sourceRepository", p.SourceRepository},
		{"note", p.Note},
	}
	for _, field := range fields {
		name, value := field.name, field.value
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s.provenance.%s is empty", path, name)
		}
	}
	if err := validateDigest(p.SourceSHA256, path+".provenance.sourceSha256"); err != nil {
		return err
	}
	if p.Classification == "synthetic" && p.SourceSHA256 != c.PRD.SHA256 {
		return fmt.Errorf("%s synthetic provenance sourceSha256 does not match its PRD", path)
	}
	if p.Classification == "derived" && c.BaseCaseID == "" {
		return fmt.Errorf("%s derived provenance requires baseCaseId", path)
	}
	if p.Classification != "derived" && c.BaseCaseID != "" {
		return fmt.Errorf("%s baseCaseId requires derived provenance", path)
	}
	return nil
}

func validateStrata(strata []string, path string) error {
	if len(strata) == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	seen := make(map[string]bool, len(strata))
	for i, stratum := range strata {
		if !stableID.MatchString(stratum) {
			return fmt.Errorf("%s[%d] %q is invalid", path, i, stratum)
		}
		if seen[stratum] {
			return fmt.Errorf("%s contains duplicate %q", path, stratum)
		}
		seen[stratum] = true
	}
	return nil
}

func validateRequirements(c *Case, path string) (map[string]bool, error) {
	seen := make(map[string]bool, len(c.Requirements))
	for i, r := range c.Requirements {
		p := fmt.Sprintf("%s.requirements[%d]", path, i)
		if !stableID.MatchString(r.ID) {
			return nil, fmt.Errorf("%s.id %q is invalid", p, r.ID)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("%s.id %q is duplicated", p, r.ID)
		}
		seen[r.ID] = true
		if !oneOf(r.Type, "capability", "constraint", "nfr", "security", "privacy", "release-gate", "dependency", "operation") {
			return nil, fmt.Errorf("%s.type %q is invalid", p, r.Type)
		}
		if !oneOf(r.Criticality, "critical", "important", "optional") {
			return nil, fmt.Errorf("%s.criticality %q is invalid", p, r.Criticality)
		}
		if strings.TrimSpace(r.Meaning) == "" || strings.TrimSpace(r.AcceptanceEvidence) == "" || strings.TrimSpace(r.AllowedGrouping) == "" {
			return nil, fmt.Errorf("%s meaning, acceptanceEvidence, and allowedGrouping must be nonempty", p)
		}
		if err := validateEvidence(c.PRD.Text, r.Evidence, p+".evidence"); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func validateOrderings(c *Case, requirements map[string]bool, path string) error {
	seen := make(map[string]bool, len(c.OrderingAxioms))
	edges := make(map[string][]string, len(requirements))
	for i, a := range c.OrderingAxioms {
		p := fmt.Sprintf("%s.orderingAxioms[%d]", path, i)
		if !stableID.MatchString(a.ID) || seen[a.ID] {
			return fmt.Errorf("%s.id %q is invalid or duplicated", p, a.ID)
		}
		seen[a.ID] = true
		if !requirements[a.Before] || !requirements[a.After] {
			return fmt.Errorf("%s references an unknown requirement", p)
		}
		if a.Before == a.After {
			return fmt.Errorf("%s orders a requirement before itself", p)
		}
		if err := validateEvidence(c.PRD.Text, a.Evidence, p+".evidence"); err != nil {
			return err
		}
		edges[a.Before] = append(edges[a.Before], a.After)
	}
	if cycle := firstCycle(requirements, edges); len(cycle) > 0 {
		return fmt.Errorf("%s.orderingAxioms contain a cycle: %s", path, strings.Join(cycle, " -> "))
	}
	return nil
}

func validateExclusions(c *Case, path string) error {
	seen := make(map[string]bool, len(c.Exclusions))
	for i, x := range c.Exclusions {
		p := fmt.Sprintf("%s.exclusions[%d]", path, i)
		if !stableID.MatchString(x.ID) || seen[x.ID] {
			return fmt.Errorf("%s.id %q is invalid or duplicated", p, x.ID)
		}
		seen[x.ID] = true
		if strings.TrimSpace(x.Meaning) == "" || strings.TrimSpace(x.ViolationTest) == "" {
			return fmt.Errorf("%s meaning and violationTest must be nonempty", p)
		}
		if err := validateEvidence(c.PRD.Text, x.Evidence, p+".evidence"); err != nil {
			return err
		}
	}
	return nil
}

func validateEvidence(prd string, spans []EvidenceSpan, path string) error {
	if len(spans) == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	for i, span := range spans {
		p := fmt.Sprintf("%s[%d]", path, i)
		if span.Start < 0 || span.End <= span.Start || span.End > len(prd) {
			return fmt.Errorf("%s [%d,%d) is outside %d PRD bytes", p, span.Start, span.End, len(prd))
		}
		if err := requireDigest(span.SHA256, []byte(prd[span.Start:span.End]), p+".sha256"); err != nil {
			return err
		}
	}
	return nil
}

func validateGranularity(g GranularityGuidance, path string) error {
	if g.MinimumTasks < 1 || g.MaximumTasks < g.MinimumTasks || g.MaximumTasks > pilotMaximumTasks {
		return fmt.Errorf("%s task bounds [%d,%d] are invalid", path, g.MinimumTasks, g.MaximumTasks)
	}
	if strings.TrimSpace(g.Guidance) == "" {
		return fmt.Errorf("%s.guidance is empty", path)
	}
	return nil
}

func validateAnnotation(a Annotation, guideVersion, split, path string) error {
	if a.GuideVersion != guideVersion {
		return fmt.Errorf("%s.guideVersion %q does not match corpus guide %q", path, a.GuideVersion, guideVersion)
	}
	if len(a.Annotators) != 2 || !normalizedIdentity(a.Annotators[0]) || !normalizedIdentity(a.Annotators[1]) || a.Annotators[0] == a.Annotators[1] {
		return fmt.Errorf("%s.annotators must name two distinct independent annotators", path)
	}
	if !normalizedIdentity(a.Adjudicator) || a.Adjudicator == a.Annotators[0] || a.Adjudicator == a.Annotators[1] {
		return fmt.Errorf("%s.adjudicator must be distinct from both annotators", path)
	}
	if a.State != "adjudicated" {
		return fmt.Errorf("%s.state %q, want adjudicated", path, a.State)
	}
	values := []struct {
		name  string
		value float64
	}{
		{"requirementMembershipKrippendorffAlpha", a.Agreement.RequirementMembership},
		{"orderingKrippendorffAlpha", a.Agreement.Ordering},
		{"granularityWeightedKappa", a.Agreement.Granularity},
	}
	for _, value := range values {
		if math.IsNaN(value.value) || math.IsInf(value.value, 0) || value.value < -1 || value.value > 1 {
			return fmt.Errorf("%s.agreement.%s is outside [-1,1]", path, value.name)
		}
	}
	if split == "calibration" && (a.Agreement.RequirementMembership < calibrationAlphaFloor || a.Agreement.Ordering < calibrationAlphaFloor || a.Agreement.Granularity < calibrationKappaFloor) {
		return fmt.Errorf("%s agreement does not meet the pilot calibration floors", path)
	}
	return nil
}

func firstCycle(nodes map[string]bool, edges map[string][]string) []string {
	const (
		unseen = iota
		active
		done
	)
	state := make(map[string]int, len(nodes))
	stack := make([]string, 0, len(nodes))
	position := make(map[string]int, len(nodes))
	var found []string
	var visit func(string) bool
	visit = func(node string) bool {
		state[node] = active
		position[node] = len(stack)
		stack = append(stack, node)
		for _, next := range edges[node] {
			switch state[next] {
			case unseen:
				if visit(next) {
					return true
				}
			case active:
				found = append(append([]string(nil), stack[position[next]:]...), next)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		delete(position, node)
		state[node] = done
		return false
	}
	ordered := make([]string, 0, len(nodes))
	for node := range nodes {
		ordered = append(ordered, node)
	}
	sort.Strings(ordered)
	for _, node := range ordered {
		if state[node] == unseen && visit(node) {
			return found
		}
	}
	return nil
}

func readJSONDocument(r io.Reader, maximumBytes int64, kind string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maximumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	if int64(len(raw)) > maximumBytes {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", kind, maximumBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%s is empty", kind)
	}
	if err := rejectForbiddenKeys(raw, kind); err != nil {
		return nil, err
	}
	return raw, nil
}

func rejectForbiddenKeys(raw []byte, kind string) error {
	var document any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&document); err != nil {
		return nil // The closed-schema decoder returns the more useful error.
	}
	var walk func(any) error
	walk = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.Map(func(r rune) rune {
					if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
						return r
					}
					if r >= 'A' && r <= 'Z' {
						return r + ('a' - 'A')
					}
					return -1
				}, key)
				if forbiddenJSONKeys[normalized] {
					return fmt.Errorf("%s contains forbidden secret-bearing field %q", kind, key)
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(document)
}

func normalizedIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func requireDigest(want string, data []byte, path string) error {
	if err := validateDigest(want, path); err != nil {
		return err
	}
	got := sha256.Sum256(data)
	if want != hex.EncodeToString(got[:]) {
		return fmt.Errorf("%s does not match the referenced bytes", path)
	}
	return nil
}

func validateDigest(value, path string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || value != strings.ToLower(value) {
		return fmt.Errorf("%s is not a lowercase SHA-256 digest", path)
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
