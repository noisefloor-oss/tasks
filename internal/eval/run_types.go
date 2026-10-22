package eval

import "github.com/noisefloor-oss/tasks/internal/genai"

const RunBundleVersion = "prd-generation-runs-v1"

// RunBundle is a complete paired evaluation over one immutable corpus split.
type RunBundle struct {
	Version        string          `json:"version"`
	EvaluationID   string          `json:"evaluationId"`
	CorpusVersion  string          `json:"corpusVersion"`
	RubricVersion  string          `json:"rubricVersion"`
	Split          string          `json:"split"`
	Repetitions    int             `json:"repetitions"`
	Configurations []Configuration `json:"configurations"`
	Runs           []Run           `json:"runs"`
}

// Configuration binds every mutable generator input for point-in-time
// comparison. Role is baseline or candidate.
type Configuration struct {
	ID                 string `json:"id"`
	Role               string `json:"role"`
	ToolCommit         string `json:"toolCommit"`
	ToolVersion        string `json:"toolVersion"`
	PromptSHA256       string `json:"promptSha256"`
	Provider           string `json:"provider"`
	RequestedModel     string `json:"requestedModel"`
	ResolvedModel      string `json:"resolvedModel"`
	ModelPinned        bool   `json:"modelPinned"`
	Transport          string `json:"transport"`
	GenerationSettings string `json:"generationSettings"`
}

// Run is one live repetition and its blind semantic adjudication.
type Run struct {
	ID                   string                 `json:"id"`
	ConfigurationID      string                 `json:"configurationId"`
	CaseID               string                 `json:"caseId"`
	Repetition           int                    `json:"repetition"`
	PRDSHA256            string                 `json:"prdSha256"`
	RequestedTasks       int                    `json:"requestedTasks"`
	StartedAt            string                 `json:"startedAt"`
	DurationMilliseconds int64                  `json:"durationMilliseconds"`
	Usage                *Usage                 `json:"usage,omitempty"`
	RawResponse          string                 `json:"rawResponse"`
	RawResponseSHA256    string                 `json:"rawResponseSha256"`
	ParsedDraftsSHA256   string                 `json:"parsedDraftsSha256"`
	Outcome              string                 `json:"outcome"`
	StopClassification   string                 `json:"stopClassification"`
	Disposition          string                 `json:"disposition"`
	Drafts               []genai.Draft          `json:"drafts"`
	Publication          PublicationObservation `json:"publication"`
	Adjudication         *Adjudication          `json:"adjudication,omitempty"`
}

// Usage is optional provider-reported accounting.
type Usage struct {
	InputTokens  int      `json:"inputTokens"`
	OutputTokens int      `json:"outputTokens"`
	CostUSD      *float64 `json:"costUsd,omitempty"`
}

// PublicationObservation records the isolated toy-project transaction result.
type PublicationObservation struct {
	Attempted         bool `json:"attempted"`
	Atomic            bool `json:"atomic"`
	PostRemapVerified bool `json:"postRemapVerified"`
}

// Adjudication is explicit semantic evidence. The grader never infers these
// mappings from task prose.
type Adjudication struct {
	Blind              bool            `json:"blind"`
	Graders            []string        `json:"graders"`
	Adjudicator        string          `json:"adjudicator"`
	TaskLabels         []TaskLabel     `json:"taskLabels"`
	OrderingLabels     []OrderingLabel `json:"orderingLabels"`
	EdgeLabels         []EdgeLabel     `json:"edgeLabels"`
	Usability          string          `json:"usability"`
	EditBurden         EditBurden      `json:"editBurden"`
	InjectionViolation bool            `json:"injectionViolation"`
}

// TaskLabel maps one 1-based generated task index to requirement units.
type TaskLabel struct {
	TaskIndex           int      `json:"taskIndex"`
	RequirementIDs      []string `json:"requirementIds"`
	Grounded            bool     `json:"grounded"`
	ExclusionViolations []string `json:"exclusionViolations"`
	Duplicate           bool     `json:"duplicate"`
	OverBroad           bool     `json:"overBroad"`
	Fragmented          bool     `json:"fragmented"`
	Actionability       float64  `json:"actionability"`
	AcceptanceQuality   float64  `json:"acceptanceQuality"`
	PriorityCalibration float64  `json:"priorityCalibration"`
}

// OrderingLabel records whether one required semantic ordering is preserved.
type OrderingLabel struct {
	AxiomID   string `json:"axiomId"`
	Satisfied bool   `json:"satisfied"`
}

// EdgeLabel records whether one generated dependency edge is justified.
type EdgeLabel struct {
	TaskIndex       int  `json:"taskIndex"`
	DependencyIndex int  `json:"dependencyIndex"`
	Justified       bool `json:"justified"`
}

// EditBurden counts material edits needed to accept a generated backlog.
type EditBurden struct {
	Added            int `json:"added"`
	Deleted          int `json:"deleted"`
	Rewritten        int `json:"rewritten"`
	Split            int `json:"split"`
	Merged           int `json:"merged"`
	Reprioritized    int `json:"reprioritized"`
	DependencyEdited int `json:"dependencyEdited"`
}
