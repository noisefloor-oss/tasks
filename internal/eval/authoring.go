package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// AuthoringCorpus is the review-friendly source form of a corpus. Evidence is
// written as exact PRD text; CompileCorpus resolves it to immutable byte spans
// and hashes and then applies the same closed validation as published data.
type AuthoringCorpus struct {
	Version                string          `json:"version"`
	RubricVersion          string          `json:"rubricVersion"`
	AnnotationGuideVersion string          `json:"annotationGuideVersion"`
	Cases                  []AuthoringCase `json:"cases"`
}

type AuthoringCase struct {
	ID                  string                 `json:"id"`
	Version             string                 `json:"version"`
	Split               string                 `json:"split"`
	ExpectedDisposition string                 `json:"expectedDisposition"`
	Provenance          Provenance             `json:"provenance"`
	Strata              []string               `json:"strata"`
	PRD                 string                 `json:"prd"`
	Requirements        []AuthoringRequirement `json:"requirements"`
	OrderingAxioms      []AuthoringOrdering    `json:"orderingAxioms"`
	Exclusions          []AuthoringExclusion   `json:"exclusions"`
	Granularity         GranularityGuidance    `json:"granularity"`
	Annotation          Annotation             `json:"annotation"`
	BaseCaseID          string                 `json:"baseCaseId,omitempty"`
}

type AuthoringRequirement struct {
	ID                 string   `json:"id"`
	Type               string   `json:"type"`
	Criticality        string   `json:"criticality"`
	Meaning            string   `json:"meaning"`
	Evidence           []string `json:"evidence"`
	AcceptanceEvidence string   `json:"acceptanceEvidence"`
	AllowedGrouping    string   `json:"allowedGrouping"`
}

type AuthoringOrdering struct {
	ID       string   `json:"id"`
	Before   string   `json:"before"`
	After    string   `json:"after"`
	Evidence []string `json:"evidence"`
	Critical bool     `json:"critical"`
}

type AuthoringExclusion struct {
	ID            string   `json:"id"`
	Meaning       string   `json:"meaning"`
	Evidence      []string `json:"evidence"`
	ViolationTest string   `json:"violationTest"`
}

// CompileCorpus reads a closed authoring document and resolves exact evidence.
func CompileCorpus(r io.Reader) (*Corpus, error) {
	raw, err := readJSONDocument(r, maximumCorpusBytes, "corpus source")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var source AuthoringCorpus
	if err := dec.Decode(&source); err != nil {
		return nil, fmt.Errorf("decode corpus source: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode corpus source: trailing JSON value")
		}
		return nil, fmt.Errorf("decode corpus source trailing data: %w", err)
	}

	corpus := &Corpus{
		Version: source.Version, RubricVersion: source.RubricVersion,
		AnnotationGuideVersion: source.AnnotationGuideVersion,
		Cases:                  make([]Case, len(source.Cases)),
	}
	for i, authored := range source.Cases {
		compiled, err := compileCase(authored, fmt.Sprintf("cases[%d]", i))
		if err != nil {
			return nil, err
		}
		corpus.Cases[i] = compiled
	}
	byID := make(map[string]*Case, len(corpus.Cases))
	for i := range corpus.Cases {
		byID[corpus.Cases[i].ID] = &corpus.Cases[i]
	}
	for i := range corpus.Cases {
		compiled := &corpus.Cases[i]
		if compiled.Provenance.SourceSHA256 != "" {
			continue
		}
		switch compiled.Provenance.Classification {
		case "synthetic":
			compiled.Provenance.SourceSHA256 = compiled.PRD.SHA256
		case "derived":
			base := byID[compiled.BaseCaseID]
			if base == nil {
				return nil, fmt.Errorf("cases[%d] cannot resolve base source hash", i)
			}
			compiled.Provenance.SourceSHA256 = base.PRD.SHA256
		}
	}
	if err := ValidateCorpus(corpus); err != nil {
		return nil, err
	}
	return corpus, nil
}

// MarshalCorpus emits the canonical resolved corpus document.
func MarshalCorpus(corpus *Corpus) ([]byte, error) {
	if err := ValidateCorpus(corpus); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func compileCase(source AuthoringCase, path string) (Case, error) {
	prdHash := digest([]byte(source.PRD))
	compiled := Case{
		ID: source.ID, Version: source.Version, Split: source.Split,
		ExpectedDisposition: source.ExpectedDisposition, Provenance: source.Provenance,
		Strata: source.Strata, PRD: PRDSnapshot{Text: source.PRD, SHA256: prdHash},
		Granularity: source.Granularity, Annotation: source.Annotation, BaseCaseID: source.BaseCaseID,
	}
	for i, requirement := range source.Requirements {
		evidence, err := compileEvidence(source.PRD, requirement.Evidence, fmt.Sprintf("%s.requirements[%d].evidence", path, i))
		if err != nil {
			return Case{}, err
		}
		compiled.Requirements = append(compiled.Requirements, Requirement{
			ID: requirement.ID, Type: requirement.Type, Criticality: requirement.Criticality,
			Meaning: requirement.Meaning, Evidence: evidence,
			AcceptanceEvidence: requirement.AcceptanceEvidence, AllowedGrouping: requirement.AllowedGrouping,
		})
	}
	for i, ordering := range source.OrderingAxioms {
		evidence, err := compileEvidence(source.PRD, ordering.Evidence, fmt.Sprintf("%s.orderingAxioms[%d].evidence", path, i))
		if err != nil {
			return Case{}, err
		}
		compiled.OrderingAxioms = append(compiled.OrderingAxioms, OrderingAxiom{
			ID: ordering.ID, Before: ordering.Before, After: ordering.After,
			Evidence: evidence, Critical: ordering.Critical,
		})
	}
	for i, exclusion := range source.Exclusions {
		evidence, err := compileEvidence(source.PRD, exclusion.Evidence, fmt.Sprintf("%s.exclusions[%d].evidence", path, i))
		if err != nil {
			return Case{}, err
		}
		compiled.Exclusions = append(compiled.Exclusions, Exclusion{
			ID: exclusion.ID, Meaning: exclusion.Meaning, Evidence: evidence,
			ViolationTest: exclusion.ViolationTest,
		})
	}
	return compiled, nil
}

func compileEvidence(prd string, evidence []string, path string) ([]EvidenceSpan, error) {
	if len(evidence) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	spans := make([]EvidenceSpan, len(evidence))
	for i, text := range evidence {
		if text == "" {
			return nil, fmt.Errorf("%s[%d] is empty", path, i)
		}
		if count := strings.Count(prd, text); count != 1 {
			return nil, fmt.Errorf("%s[%d] occurs %d times in the PRD, want exactly once", path, i, count)
		}
		start := strings.Index(prd, text)
		spans[i] = EvidenceSpan{Start: start, End: start + len(text), SHA256: digest([]byte(text))}
	}
	return spans, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
