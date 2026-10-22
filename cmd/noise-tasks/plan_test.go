package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

const cliPlan = `{"schemaVersion":1,"planId":"cli-plan","tag":"main","milestones":[{"key":"m","name":"next","status":"open"}],"tasks":[{"key":"a","title":"A","description":"","details":"","priority":"high","milestone":{"key":"m"},"dependencies":[{"id":"1"}]},{"key":"b","title":"B","description":"","details":"","priority":"medium","dependencies":[{"key":"a"}]}],"assignments":[{"key":"owner","task":{"key":"a"},"owner":"agent-a"}]}`

func cliPlanProject(t *testing.T) string {
	t.Helper()
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(cliPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanValidateAndApplyCLIJSON(t *testing.T) {
	path := cliPlanProject(t)
	code, stdout, stderr := runCmd(cmdPlan, "validate", path, "--json")
	if code != 0 {
		t.Fatalf("validate exit %d: %s", code, stderr)
	}
	var validation store.OrchestratorPlanValidation
	if err := json.Unmarshal([]byte(stdout), &validation); err != nil {
		t.Fatal(err)
	}
	if validation.Offline || validation.PlanID != "cli-plan" || validation.Inventory.Tasks != 2 || !store.ValidDigest(validation.PlanDigest) {
		t.Fatalf("validation = %+v", validation)
	}

	code, stdout, stderr = runCmd(cmdPlan, "apply", path, "--json", "--digest", validation.PlanDigest, "--request-id", "cli-req")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	var applied store.OrchestratorPlanApplyResult
	if err := json.Unmarshal([]byte(stdout), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Replayed || applied.Receipt["planId"] != "cli-plan" {
		t.Fatalf("apply = %+v", applied)
	}
	code, stdout, stderr = runCmd(cmdPlan, "apply", "--json", "--digest", validation.PlanDigest, "--request-id", "cli-req", path)
	if code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &applied); err != nil || !applied.Replayed {
		t.Fatalf("replay = %+v, %v", applied, err)
	}
}

func TestPlanValidateOfflineDoesNotNeedProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(cliPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOISE_PROJECT_DIR", t.TempDir())
	code, stdout, stderr := runCmd(cmdPlan, "validate", "--offline", "--json", path)
	var validation store.OrchestratorPlanValidation
	decodeErr := json.Unmarshal([]byte(stdout), &validation)
	if code != 0 || stderr != "" || decodeErr != nil || !validation.Offline {
		t.Fatalf("offline = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestPlanValidateHumanIncludesSchemaVersion(t *testing.T) {
	path := cliPlanProject(t)
	code, stdout, stderr := runCmd(cmdPlan, "validate", "--offline", path)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "schemaVersion: 1\n") {
		t.Fatalf("human validate = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestPlanCLIUsageAndConflicts(t *testing.T) {
	path := cliPlanProject(t)
	p, err := store.ReadOrchestratorPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		nil,
		{"validate"},
		{"apply", path},
		{"apply", "--digest", p.Digest, path},
		{"apply", "--digest", "nope", "--request-id", "r", path},
	} {
		if code, stdout, _ := runCmd(cmdPlan, args...); code != 2 || stdout != "" {
			t.Fatalf("args %v = exit %d stdout %q", args, code, stdout)
		}
	}
	if code, _, stderr := runCmd(cmdPlan, "apply", "--digest", p.Digest, "--request-id", "r1", path); code != 0 {
		t.Fatalf("first apply: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdPlan, "apply", "--digest", p.Digest, "--request-id", "r2", path); code != 3 || stdout != "" || !strings.Contains(stderr, "plan id") {
		t.Fatalf("plan-id conflict = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
