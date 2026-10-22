package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestLoadCorpus(t *testing.T) {
	raw := marshalCorpus(t, validCorpus(t))
	got, err := LoadCorpus(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	if len(got.Cases) != 12 || got.Cases[0].ID != "calibration-1" {
		t.Fatalf("loaded unexpected corpus: %#v", got.Cases)
	}
}

func TestLoadCorpusClosedDocument(t *testing.T) {
	raw := marshalCorpus(t, validCorpus(t))
	raw = bytes.Replace(raw, []byte(`"rubricVersion":`), []byte(`"unknown":true,"rubricVersion":`), 1)
	if _, err := LoadCorpus(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}

	raw = append(marshalCorpus(t, validCorpus(t)), []byte(` {"second":true}`)...)
	if _, err := LoadCorpus(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing document error = %v", err)
	}
}

func TestLoadCorpusRejectsSecretBearingField(t *testing.T) {
	raw := marshalCorpus(t, validCorpus(t))
	raw = bytes.Replace(raw, []byte(`"rubricVersion":`), []byte(`"api_key":"not-a-real-key","rubricVersion":`), 1)
	if _, err := LoadCorpus(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "secret-bearing field") {
		t.Fatalf("secret field error = %v", err)
	}
}

func TestValidateCorpusInvariants(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Corpus)
		want string
	}{
		{"version", func(c *Corpus) { c.Version = "moving" }, "corpus version"},
		{"case count", func(c *Corpus) { c.Cases = c.Cases[:11] }, "exactly 12"},
		{"split count", func(c *Corpus) { c.Cases[0].Split = "development" }, "split \"calibration\""},
		{"duplicate case", func(c *Corpus) { c.Cases[1].ID = c.Cases[0].ID }, "duplicated"},
		{"prd hash", func(c *Corpus) {
			c.Cases[0].PRD.SHA256 = strings.Repeat("0", 64)
			c.Cases[0].Provenance.SourceSHA256 = c.Cases[0].PRD.SHA256
		}, "referenced bytes"},
		{"span bounds", func(c *Corpus) { c.Cases[0].Requirements[0].Evidence[0].End = 10000 }, "outside"},
		{"span hash", func(c *Corpus) { c.Cases[0].Requirements[0].Evidence[0].SHA256 = strings.Repeat("0", 64) }, "referenced bytes"},
		{"requirement reference", func(c *Corpus) { c.Cases[0].OrderingAxioms[0].After = "missing" }, "unknown requirement"},
		{"ordering cycle", func(c *Corpus) {
			cs := &c.Cases[0]
			cs.OrderingAxioms = append(cs.OrderingAxioms, OrderingAxiom{
				ID: "reverse-order", Before: "service", After: "storage",
				Evidence: cs.Requirements[0].Evidence,
			})
		}, "contain a cycle"},
		{"base missing", func(c *Corpus) { c.Cases[11].BaseCaseID = "missing" }, "does not exist"},
		{"base cross split", func(c *Corpus) {
			c.Cases[11].BaseCaseID = "development-1"
			c.Cases[11].Provenance.SourceSHA256 = c.Cases[4].PRD.SHA256
		}, "different splits"},
		{"base source hash", func(c *Corpus) { c.Cases[11].Provenance.SourceSHA256 = strings.Repeat("0", 64) }, "does not match base"},
		{"synthetic source hash", func(c *Corpus) { c.Cases[0].Provenance.SourceSHA256 = strings.Repeat("0", 64) }, "does not match its PRD"},
		{"base cycle", func(c *Corpus) {
			c.Cases[10].Provenance.Classification = "derived"
			c.Cases[10].BaseCaseID = c.Cases[11].ID
			c.Cases[10].Provenance.SourceSHA256 = c.Cases[11].PRD.SHA256
		}, "contain a cycle"},
		{"annotators", func(c *Corpus) { c.Cases[0].Annotation.Annotators[1] = c.Cases[0].Annotation.Annotators[0] }, "distinct independent"},
		{"annotator whitespace", func(c *Corpus) { c.Cases[0].Annotation.Annotators[1] += " " }, "distinct independent"},
		{"agreement floor", func(c *Corpus) { c.Cases[0].Annotation.Agreement.Ordering = .79 }, "calibration floors"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			corpus := cloneCorpus(t, validCorpus(t))
			tc.edit(corpus)
			err := ValidateCorpus(corpus)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateCorpus error = %v, want %q", err, tc.want)
			}
		})
	}
}

func validCorpus(t *testing.T) *Corpus {
	t.Helper()
	corpus := &Corpus{
		Version:                CorpusVersion,
		RubricVersion:          RubricVersion,
		AnnotationGuideVersion: "annotation-guide-v1",
	}
	for _, split := range []string{"calibration", "development", "locked"} {
		for i := 1; i <= 4; i++ {
			id := split + "-" + string(rune('0'+i))
			corpus.Cases = append(corpus.Cases, validCase(t, id, split))
		}
	}
	derived := &corpus.Cases[11]
	derived.Provenance.Classification = "derived"
	derived.BaseCaseID = "locked-3"
	derived.Provenance.SourceSHA256 = corpus.Cases[10].PRD.SHA256
	return corpus
}

func validCase(t *testing.T, id, split string) Case {
	t.Helper()
	text := "Create durable storage for " + id + ". Build the service only after storage is ready. Do not publish operator secrets."
	storageText := "Create durable storage for " + id + "."
	serviceText := "Build the service only after storage is ready."
	exclusionText := "Do not publish operator secrets."
	storage := spanFor(t, text, storageText)
	service := spanFor(t, text, serviceText)
	exclusion := spanFor(t, text, exclusionText)
	digest := digestString(text)
	return Case{
		ID: id, Version: "1.0.0", Split: split, ExpectedDisposition: "generate",
		Provenance: Provenance{
			Classification: "synthetic", SourceRepository: "internal-suite",
			SourceSHA256: digest, Note: "Synthetic case; no private source text is retained.",
		},
		Strata: []string{"cli", "short", "explicit", "linear"},
		PRD:    PRDSnapshot{Text: text, SHA256: digest},
		Requirements: []Requirement{
			{ID: "storage", Type: "capability", Criticality: "critical", Meaning: "Create durable storage.", Evidence: []EvidenceSpan{storage}, AcceptanceEvidence: "Storage survives restart.", AllowedGrouping: "May share a task with storage schema work."},
			{ID: "service", Type: "capability", Criticality: "important", Meaning: "Build the service after storage.", Evidence: []EvidenceSpan{service}, AcceptanceEvidence: "Service uses ready storage.", AllowedGrouping: "May be split by interface and implementation."},
		},
		OrderingAxioms: []OrderingAxiom{{ID: "storage-before-service", Before: "storage", After: "service", Evidence: []EvidenceSpan{service}, Critical: true}},
		Exclusions:     []Exclusion{{ID: "no-secret-publication", Meaning: "Operator secrets must not be published.", Evidence: []EvidenceSpan{exclusion}, ViolationTest: "Any task proposing publication of secrets violates the exclusion."}},
		Granularity:    GranularityGuidance{MinimumTasks: 2, MaximumTasks: 4, Guidance: "Keep storage and service ownership reviewable."},
		Annotation: Annotation{
			GuideVersion: "annotation-guide-v1", Annotators: []string{"annotator-a", "annotator-b"},
			Adjudicator: "adjudicator-c", State: "adjudicated",
			Agreement: Agreement{RequirementMembership: .9, Ordering: .9, Granularity: .8},
		},
	}
}

func spanFor(t *testing.T, text, evidence string) EvidenceSpan {
	t.Helper()
	start := strings.Index(text, evidence)
	if start < 0 {
		t.Fatalf("evidence %q not found", evidence)
	}
	return EvidenceSpan{Start: start, End: start + len(evidence), SHA256: digestString(evidence)}
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func marshalCorpus(t *testing.T, corpus *Corpus) []byte {
	t.Helper()
	raw, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func cloneCorpus(t *testing.T, corpus *Corpus) *Corpus {
	t.Helper()
	var clone Corpus
	if err := json.Unmarshal(marshalCorpus(t, corpus), &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}
