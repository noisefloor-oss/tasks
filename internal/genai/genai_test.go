package genai

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDraftsRejectsDependencyCycles(t *testing.T) {
	drafts := []Draft{
		{Title: "one", Dependencies: []int{2}},
		{Title: "two", Dependencies: []int{3}},
		{Title: "three", Dependencies: []int{1}},
	}
	err := ValidateDrafts(drafts)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestValidateResultRejectsUnsafeResolvedIdentity(t *testing.T) {
	result := Result{Drafts: []Draft{{Title: "one"}}, ResolvedModel: strings.Repeat("m", maxModelBytes+1)}
	if err := ValidateResult(result); err == nil || !strings.Contains(err.Error(), "resolved model") {
		t.Fatalf("resolved identity error = %v", err)
	}
}

func TestValidateDraftsGeneratedTextContract(t *testing.T) {
	valid := Draft{Title: "Add the flag", Description: "Adds a flag to the CLI.", Priority: "high"}

	if err := ValidateDrafts([]Draft{valid}); err != nil {
		t.Fatalf("valid draft rejected: %v", err)
	}

	multiline := valid
	multiline.Title = "line one\nline two"
	if err := ValidateDrafts([]Draft{multiline}); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("multiline title = %v", err)
	}

	longTitle := valid
	longTitle.Title = strings.Repeat("x", 201)
	if err := ValidateDrafts([]Draft{longTitle}); err == nil || !strings.Contains(err.Error(), "200 bytes") {
		t.Fatalf("long title = %v", err)
	}

	multilineDesc := valid
	multilineDesc.Description = "one.\rtwo."
	if err := ValidateDrafts([]Draft{multilineDesc}); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("multiline description = %v", err)
	}

	longDesc := valid
	longDesc.Description = strings.Repeat("y", 501)
	if err := ValidateDrafts([]Draft{longDesc}); err == nil || !strings.Contains(err.Error(), "500 bytes") {
		t.Fatalf("long description = %v", err)
	}

	// Exact-boundary acceptance.
	atTitle := valid
	atTitle.Title = strings.Repeat("x", 200)
	if err := ValidateDrafts([]Draft{atTitle}); err != nil {
		t.Fatalf("200-byte title rejected: %v", err)
	}
	atDesc := valid
	atDesc.Description = strings.Repeat("y", 500)
	if err := ValidateDrafts([]Draft{atDesc}); err != nil {
		t.Fatalf("500-byte description rejected: %v", err)
	}

	// Multibyte byte counting: 201 bytes in runes under 200 runes.
	multibyte := valid
	multibyte.Title = strings.Repeat("é", 101) // 202 bytes
	if err := ValidateDrafts([]Draft{multibyte}); err == nil || !strings.Contains(err.Error(), "200 bytes") {
		t.Fatalf("multibyte title = %v", err)
	}

	// Oversize details are advisory: never a batch refusal.
	bigDetails := valid
	bigDetails.Details = strings.Repeat("z", 16<<10)
	if err := ValidateDrafts([]Draft{bigDetails}); err != nil {
		t.Fatalf("oversize details rejected: %v", err)
	}
}

func TestFinishDraftsEmptyIsTypedRefusal(t *testing.T) {
	_, err := finishDrafts("[]", 10)
	if err == nil {
		t.Fatal("empty array accepted")
	}
	if !errors.Is(err, ErrPRDNotActionable) {
		t.Fatalf("empty array error = %v, want ErrPRDNotActionable", err)
	}

	// A non-empty valid batch still succeeds.
	drafts, err := finishDrafts(`[{"title":"A","description":"d","details":"","priority":"high","dependencies":[]}]`, 10)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("valid batch = %v, %v", drafts, err)
	}
}
