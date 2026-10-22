// Package eval validates and scores frozen PRD-generation evaluations.
// It is deliberately offline: live provider execution belongs outside this
// package and is recorded through immutable run manifests.
package eval

const (
	// CorpusVersion is the closed schema version for the twelve-case pilot.
	CorpusVersion = "prd-generation-pilot-v1"
	// RubricVersion binds labels and reports to the first frozen rubric.
	RubricVersion = "prd-generation-rubric-v1"

	pilotCaseCount        = 12
	pilotCasesPerSplit    = 4
	pilotMaximumTasks     = 50
	maximumCorpusBytes    = 16 << 20
	maximumRunBundleBytes = 64 << 20
	calibrationAlphaFloor = 0.80
	calibrationKappaFloor = 0.67
)

// Corpus is a versioned set of PRD cases and adjudicated requirement labels.
type Corpus struct {
	Version                string `json:"version"`
	RubricVersion          string `json:"rubricVersion"`
	AnnotationGuideVersion string `json:"annotationGuideVersion"`
	Cases                  []Case `json:"cases"`
}

// Case is one independent PRD cluster. Repetitions of a case are not
// independent samples.
type Case struct {
	ID                  string              `json:"id"`
	Version             string              `json:"version"`
	Split               string              `json:"split"`
	ExpectedDisposition string              `json:"expectedDisposition"`
	Provenance          Provenance          `json:"provenance"`
	Strata              []string            `json:"strata"`
	PRD                 PRDSnapshot         `json:"prd"`
	Requirements        []Requirement       `json:"requirements"`
	OrderingAxioms      []OrderingAxiom     `json:"orderingAxioms"`
	Exclusions          []Exclusion         `json:"exclusions"`
	Granularity         GranularityGuidance `json:"granularity"`
	Annotation          Annotation          `json:"annotation"`
	BaseCaseID          string              `json:"baseCaseId,omitempty"`
}

// Provenance binds a sanitized or synthetic snapshot to its declared origin.
// SourceRepository is a neutral source label.
// SourceSHA256 is the source snapshot hash when one may be disclosed; for a
// fully synthetic case it is the committed PRD hash.
type Provenance struct {
	Classification   string `json:"classification"`
	SourceRepository string `json:"sourceRepository"`
	SourceSection    string `json:"sourceSection,omitempty"`
	SourceSHA256     string `json:"sourceSha256"`
	Note             string `json:"note"`
}

// PRDSnapshot is the exact input evaluated by a case.
type PRDSnapshot struct {
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}

// EvidenceSpan addresses UTF-8 PRD bytes using a half-open [start,end)
// interval. SHA256 hashes precisely those bytes.
type EvidenceSpan struct {
	Start  int    `json:"start"`
	End    int    `json:"end"`
	SHA256 string `json:"sha256"`
}

// Requirement is one invariant semantic unit projected from a generated
// backlog. It is intentionally not a reference task.
type Requirement struct {
	ID                 string         `json:"id"`
	Type               string         `json:"type"`
	Criticality        string         `json:"criticality"`
	Meaning            string         `json:"meaning"`
	Evidence           []EvidenceSpan `json:"evidence"`
	AcceptanceEvidence string         `json:"acceptanceEvidence"`
	AllowedGrouping    string         `json:"allowedGrouping"`
}

// OrderingAxiom states a required semantic ordering between requirements.
type OrderingAxiom struct {
	ID       string         `json:"id"`
	Before   string         `json:"before"`
	After    string         `json:"after"`
	Evidence []EvidenceSpan `json:"evidence"`
	Critical bool           `json:"critical"`
}

// Exclusion is an explicitly forbidden behavior and its observable test.
type Exclusion struct {
	ID            string         `json:"id"`
	Meaning       string         `json:"meaning"`
	Evidence      []EvidenceSpan `json:"evidence"`
	ViolationTest string         `json:"violationTest"`
}

// GranularityGuidance gives acceptable bounds without prescribing one exact
// decomposition.
type GranularityGuidance struct {
	MinimumTasks int    `json:"minimumTasks"`
	MaximumTasks int    `json:"maximumTasks"`
	Guidance     string `json:"guidance"`
}

// Annotation records independent labeling and adjudication provenance.
type Annotation struct {
	GuideVersion string    `json:"guideVersion"`
	Annotators   []string  `json:"annotators"`
	Adjudicator  string    `json:"adjudicator"`
	State        string    `json:"state"`
	Agreement    Agreement `json:"agreement"`
}

// Agreement contains the statistics named by the evaluation contract.
type Agreement struct {
	RequirementMembership float64 `json:"requirementMembershipKrippendorffAlpha"`
	Ordering              float64 `json:"orderingKrippendorffAlpha"`
	Granularity           float64 `json:"granularityWeightedKappa"`
}
