package eval

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCompileCorpusRoundTrip(t *testing.T) {
	want := validCorpus(t)
	source := authoringFromCorpus(want)
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CompileCorpus(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("CompileCorpus: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compiled corpus differs\ngot:  %#v\nwant: %#v", got, want)
	}
	first, err := MarshalCorpus(got)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalCorpus(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || first[len(first)-1] != '\n' {
		t.Fatal("resolved corpus JSON is not deterministic")
	}
}

func TestCompileCorpusRequiresUniqueEvidenceText(t *testing.T) {
	source := authoringFromCorpus(validCorpus(t))
	caseDef := &source.Cases[0]
	caseDef.PRD = "repeat repeat"
	caseDef.Requirements[0].Evidence = []string{"repeat"}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileCorpus(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("ambiguous evidence error = %v", err)
	}
}

func TestCompileCorpusClosedSchema(t *testing.T) {
	source := authoringFromCorpus(validCorpus(t))
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"rubricVersion":`), []byte(`"extra":true,"rubricVersion":`), 1)
	if _, err := CompileCorpus(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func authoringFromCorpus(corpus *Corpus) *AuthoringCorpus {
	source := &AuthoringCorpus{
		Version: corpus.Version, RubricVersion: corpus.RubricVersion,
		AnnotationGuideVersion: corpus.AnnotationGuideVersion,
		Cases:                  make([]AuthoringCase, len(corpus.Cases)),
	}
	for i, compiled := range corpus.Cases {
		authored := AuthoringCase{
			ID: compiled.ID, Version: compiled.Version, Split: compiled.Split,
			ExpectedDisposition: compiled.ExpectedDisposition, Provenance: compiled.Provenance,
			Strata: compiled.Strata, PRD: compiled.PRD.Text, Granularity: compiled.Granularity,
			Annotation: compiled.Annotation, BaseCaseID: compiled.BaseCaseID,
		}
		for _, requirement := range compiled.Requirements {
			authored.Requirements = append(authored.Requirements, AuthoringRequirement{
				ID: requirement.ID, Type: requirement.Type, Criticality: requirement.Criticality,
				Meaning: requirement.Meaning, Evidence: evidenceText(compiled.PRD.Text, requirement.Evidence),
				AcceptanceEvidence: requirement.AcceptanceEvidence, AllowedGrouping: requirement.AllowedGrouping,
			})
		}
		for _, ordering := range compiled.OrderingAxioms {
			authored.OrderingAxioms = append(authored.OrderingAxioms, AuthoringOrdering{
				ID: ordering.ID, Before: ordering.Before, After: ordering.After,
				Evidence: evidenceText(compiled.PRD.Text, ordering.Evidence), Critical: ordering.Critical,
			})
		}
		for _, exclusion := range compiled.Exclusions {
			authored.Exclusions = append(authored.Exclusions, AuthoringExclusion{
				ID: exclusion.ID, Meaning: exclusion.Meaning,
				Evidence: evidenceText(compiled.PRD.Text, exclusion.Evidence), ViolationTest: exclusion.ViolationTest,
			})
		}
		source.Cases[i] = authored
	}
	return source
}

func evidenceText(prd string, spans []EvidenceSpan) []string {
	result := make([]string, len(spans))
	for i, span := range spans {
		result[i] = prd[span.Start:span.End]
	}
	return result
}
