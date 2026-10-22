//go:build ignore

// Generates the synthetic fixtures under testdata/. Deterministic: fixed
// timestamps, no randomness. Run from the testdata directory:
//
//	go run gen_fixtures.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var baseDir string

var baseTime = time.Date(2025, 5, 10, 8, 0, 0, 0, time.UTC)

func stamp(i int) string {
	return baseTime.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
}

func main() {
	var err error
	baseDir, err = os.Getwd()
	if err != nil {
		panic(err)
	}

	genBasic()
	genLegacyMaster()
	genTwoTags()
	genUnknownFields()
	genLegacyMeta()
	genValidationFixtures()
}

func genBasic() {
	tasks := make([]any, 100)
	statuses := []string{"pending", "pending", "in-progress", "review", "done", "deferred", "cancelled", "blocked"}
	priorities := []string{"high", "medium", "low"}

	for i := range tasks {
		deps := []string{}
		if i > 0 && i%5 == 0 {
			deps = append(deps, fmt.Sprintf("%d", i))
		}
		if i > 2 && i%7 == 0 {
			deps = append(deps, fmt.Sprintf("%d", i-1))
		}

		details := ""
		if i%6 == 2 {
			details = fmt.Sprintf(
				"Steps:\n1. Draft & review the approach\n2. Implement <carefully>\n\n<info added %s>\nImported from the planning doc; scope trimmed to v1.\n</info>",
				stamp(i))
		}

		t := task{
			ID:           fmt.Sprintf("%d", i+1),
			Title:        fmt.Sprintf("Task %d: %s", i+1, taskTitles[i%len(taskTitles)]),
			Description:  taskDescriptions[i%len(taskDescriptions)],
			Details:      details,
			Status:       statuses[i%len(statuses)],
			Priority:     priorities[i%len(priorities)],
			Dependencies: deps,
			Subtasks:     []any{},
			UpdatedAt:    stamp(i),
		}
		if i%9 == 3 {
			t.Subtasks = []any{
				map[string]any{
					"id":           1,
					"title":        fmt.Sprintf("Spike for task %d", i+1),
					"description":  "Short investigation before the main work.",
					"details":      "",
					"status":       "pending",
					"dependencies": []string{},
					"parentTaskId": i + 1,
				},
			}
		}
		tasks[i] = t
	}

	data := map[string]any{
		"main": map[string]any{
			"tasks":    tasks,
			"metadata": makeMetadata("main", len(tasks), 12),
		},
	}
	writeJSON("roundtrip/basic.json", data)
}

func genLegacyMaster() {
	type legacyTask struct {
		ID           int    `json:"id"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Status       string `json:"status"`
		Priority     string `json:"priority"`
		Dependencies []int  `json:"dependencies"`
		Subtasks     []any  `json:"subtasks"`
		UpdatedAt    string `json:"updatedAt"`
	}

	tasks := make([]legacyTask, 30)
	statuses := []string{"pending", "in-progress", "done", "deferred", "cancelled", "blocked"}
	priorities := []string{"high", "medium", "low"}

	done := 0
	for i := range tasks {
		deps := []int{}
		if i > 0 && i%4 == 0 {
			deps = append(deps, i)
		}
		status := statuses[i%len(statuses)]
		if status == "done" {
			done++
		}
		tasks[i] = legacyTask{
			ID:           i + 1,
			Title:        fmt.Sprintf("Legacy Task %d: %s", i+1, taskTitles[i%len(taskTitles)]),
			Description:  "",
			Status:       status,
			Priority:     priorities[i%len(priorities)],
			Dependencies: deps,
			Subtasks:     []any{},
			UpdatedAt:    "2024-06-15T12:00:00Z",
		}
	}

	data := map[string]any{
		"master": map[string]any{
			"tasks":    tasks,
			"metadata": makeMetadata("master", len(tasks), done),
		},
	}
	writeJSON("roundtrip/legacy_master.json", data)
}

func genTwoTags() {
	tasksMain := make([]task, 10)
	for i := range tasksMain {
		tasksMain[i] = task{
			ID:           fmt.Sprintf("%d", i+1),
			Title:        fmt.Sprintf("Main Task %d", i+1),
			Description:  "",
			Status:       "pending",
			Priority:     "medium",
			Dependencies: []string{},
			Subtasks:     []any{},
			UpdatedAt:    stamp(i),
		}
	}

	tasksDev := make([]task, 5)
	for i := range tasksDev {
		tasksDev[i] = task{
			ID:           fmt.Sprintf("%d", i+1),
			Title:        fmt.Sprintf("Dev Task %d", i+1),
			Description:  "",
			Status:       "in-progress",
			Priority:     "high",
			Dependencies: []string{},
			Subtasks:     []any{},
			UpdatedAt:    stamp(i),
		}
	}

	data := map[string]any{
		"main": map[string]any{
			"tasks":    tasksMain,
			"metadata": makeMetadata("main", len(tasksMain), 0),
		},
		"dev": map[string]any{
			"tasks":    tasksDev,
			"metadata": makeMetadata("dev", len(tasksDev), 0),
		},
	}
	writeJSON("roundtrip/two_tags.json", data)
}

func genUnknownFields() {
	tasks := make([]any, 3)

	subtasks := []any{
		map[string]any{
			"id":            1,
			"title":         "Subtask 1",
			"description":   "",
			"details":       "",
			"status":        "pending",
			"dependencies":  []string{},
			"parentTaskId":  "1",
			"_custom_field": "extra data here",
		},
	}

	tasks[0] = map[string]any{
		"id":           "1",
		"title":        "Task with unknown fields",
		"description":  "Has extra fields at task level",
		"details":      "",
		"status":       "pending",
		"priority":     "high",
		"dependencies": []string{},
		"subtasks":     subtasks,
		"updatedAt":    stamp(0),
		"_x_meta":      map[string]any{"source": "import", "id": 42},
		"_legacy_flag": true,
	}

	tasks[1] = map[string]any{
		"id":           "2",
		"title":        "Normal task",
		"description":  "No extra fields",
		"details":      "<info added 2025-05-10T08:05:00Z>\nDetails with <angle brackets> & ampersands survive round-trip.\n</info>",
		"status":       "done",
		"priority":     "medium",
		"dependencies": []string{"1"},
		"subtasks":     []any{},
		"updatedAt":    stamp(1),
	}

	tasks[2] = map[string]any{
		"id":           "3",
		"title":        "Another task with extras",
		"description":  "Has extra at both levels",
		"details":      "",
		"status":       "in-progress",
		"priority":     "low",
		"dependencies": []string{},
		"subtasks":     []any{},
		"updatedAt":    stamp(2),
		"_color":       "blue",
		"_points":      5,
	}

	meta := makeMetadata("main", len(tasks), 1)
	meta["_generated_by"] = "test-script"
	meta["_version_info"] = map[string]any{"semver": "1.0.0", "build": 123}

	data := map[string]any{
		"main": map[string]any{
			"tasks":          tasks,
			"metadata":       meta,
			"_custom_prefix": "ns",
		},
	}
	writeJSON("roundtrip/unknown_fields.json", data)
}

func genLegacyMeta() {
	tasks := make([]task, 3)
	for i := range tasks {
		tasks[i] = task{
			ID:           fmt.Sprintf("%d", i+1),
			Title:        fmt.Sprintf("Task %d", i+1),
			Description:  fmt.Sprintf("Description %d", i+1),
			Status:       "pending",
			Priority:     "high",
			Dependencies: []string{},
			Subtasks:     []any{},
			UpdatedAt:    "2025-01-01T00:00:00Z",
		}
	}

	meta := map[string]any{
		"version":      "0.5.0",
		"created":      "2024-12-01T00:00:00Z",
		"lastModified": "2025-01-01T00:00:00Z",
		"taskCount":    3,
		"tags":         []string{"main"},
		"_legacy_key":  "preserved-value",
	}

	data := map[string]any{
		"main": map[string]any{
			"tasks":    tasks,
			"metadata": meta,
		},
	}
	writeJSON("roundtrip/legacy_meta.json", data)
}

func genValidationFixtures() {
	dupTasks := []task{
		{ID: "1", Title: "Task 1", Status: "pending", Priority: "high", UpdatedAt: stamp(0), Dependencies: []string{}, Subtasks: []any{}},
		{ID: "1", Title: "Task 1 dup", Status: "pending", Priority: "high", UpdatedAt: stamp(1), Dependencies: []string{}, Subtasks: []any{}},
	}
	data := map[string]any{
		"main": map[string]any{
			"tasks":    dupTasks,
			"metadata": makeMetadata("main", 2, 0),
		},
	}
	writeJSON("validation/dup_ids.json", data)

	badStatusTasks := []task{
		{ID: "1", Title: "Task 1", Status: "unknown-status", Priority: "high", UpdatedAt: stamp(0), Dependencies: []string{}, Subtasks: []any{}},
	}
	data = map[string]any{
		"main": map[string]any{
			"tasks":    badStatusTasks,
			"metadata": makeMetadata("main", 1, 0),
		},
	}
	writeJSON("validation/bad_status.json", data)

	badIDTasks := []task{
		{ID: "abc", Title: "Task 1", Status: "pending", Priority: "high", UpdatedAt: stamp(0), Dependencies: []string{}, Subtasks: []any{}},
	}
	data = map[string]any{
		"main": map[string]any{
			"tasks":    badIDTasks,
			"metadata": makeMetadata("main", 1, 0),
		},
	}
	writeJSON("validation/bad_id.json", data)

	badDepTasks := []task{
		{ID: "1", Title: "Task 1", Status: "pending", Priority: "high", Dependencies: []string{"xyz"}, Subtasks: []any{}, UpdatedAt: stamp(0)},
	}
	data = map[string]any{
		"main": map[string]any{
			"tasks":    badDepTasks,
			"metadata": makeMetadata("main", 1, 0),
		},
	}
	writeJSON("validation/bad_dep_id.json", data)
}

type task struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Details      string   `json:"details"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	Dependencies []string `json:"dependencies"`
	Subtasks     []any    `json:"subtasks"`
	UpdatedAt    string   `json:"updatedAt"`
}

func makeMetadata(tag string, count, completed int) map[string]any {
	return map[string]any{
		"version":        "1.0.0",
		"lastModified":   baseTime.Add(time.Hour).Format(time.RFC3339),
		"taskCount":      count,
		"completedCount": completed,
		"tags":           []string{tag},
	}
}

func writeJSON(relPath string, v any) {
	path := filepath.Join(baseDir, relPath)
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
}

var taskTitles = []string{
	"Set up project scaffolding",
	"Implement user authentication",
	"Design database schema",
	"Create API endpoints",
	"Write unit tests",
	"Set up CI/CD pipeline",
	"Configure logging system",
	"Implement caching layer",
	"Add rate limiting",
	"Create documentation",
	"Refactor legacy module",
	"Add health check endpoint",
	"Implement search functionality",
	"Set up monitoring dashboard",
	"Configure error tracking",
	"Add data validation",
	"Implement file upload",
	"Create email notification system",
	"Add pagination to list endpoints",
	"Set up backup strategy",
}

var taskDescriptions = []string{
	"Core infrastructure setup required for the project foundation.",
	"Implement secure user login and session management.",
	"Design normalized schema supporting all current and planned features.",
	"RESTful endpoints following OpenAPI specification.",
	"Comprehensive test coverage for critical business logic.",
	"Automated build, test, and deployment workflow.",
	"Structured logging with correlation IDs and log levels.",
	"In-memory cache with Redis fallback for high-traffic queries.",
	"Per-endpoint rate limiting with configurable thresholds.",
	"API reference and developer onboarding guide.",
	"Simplify the module interface and remove dead code paths.",
	"Kubernetes-compatible health and readiness probes.",
	"Full-text search across documents with relevance scoring.",
	"Real-time system metrics with customizable dashboards.",
	"Sentry integration for production error tracking.",
	"Input sanitization and schema-based request validation.",
	"Multipart upload with progress tracking and virus scanning.",
	"Transactional email templates with provider integration.",
	"Cursor-based pagination for large result sets.",
	"Automated daily backups with point-in-time recovery.",
}
