package store

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestHelperProcess is the multi-process worker (SPEC §8.8: real processes,
// not goroutines sharing one connection). The parent re-executes the test
// binary with NOISE_STORE_HELPER=1; the first argument after "--" selects
// the operation, the rest are its arguments.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("NOISE_STORE_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+2 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper arguments")
		os.Exit(2)
	}
	args := os.Args[separator+1:]
	db, err := Open(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	switch args[0] {
	case "add":
		id, err := db.AddTask("main", &Task{Title: args[2]})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(id)
	case "cas":
		err := db.UpdateTask("main", args[2], atoi(args[3]), nil, func(task *Task) error {
			task.Details += "touched by " + args[4] + "\n"
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Println("won")
	case "claim":
		claim, _, _, err := db.AcquireClaim("main", args[2], args[3], 300, args[4])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Println(claim.Token)
	case "claim-next":
		task, claim, _, err := db.ClaimNext("main", args[2], 300, "", "", args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		if task == nil {
			fmt.Println("empty")
		} else {
			fmt.Println(task.ID + " " + fmt.Sprint(claim.Token))
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown helper op "+args[0])
		os.Exit(2)
	}
	os.Exit(0)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func spawnHelper(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var outBuf, errBuf strings.Builder
	cmd := exec.Command(executable, append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "NOISE_STORE_HELPER=1")
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	code = 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("helper: %v", err)
	}
	return outBuf.String(), errBuf.String(), code
}

func TestConcurrentProcessAddsAllocateUniqueIDs(t *testing.T) {
	db, path := newDB(t)
	db.Close()

	const writers = 12
	type result struct {
		stdout, stderr string
		code           int
	}
	results := make(chan result, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			stdout, stderr, code := spawnHelper(t, "add", path, fmt.Sprintf("process-%02d", i))
			results <- result{stdout, stderr, code}
		}(i)
	}
	seen := make(map[string]bool, writers)
	for i := 0; i < writers; i++ {
		r := <-results
		if r.code != 0 {
			t.Fatalf("writer failed: exit %d: %s", r.code, r.stderr)
		}
		id := strings.TrimSpace(r.stdout)
		if seen[id] {
			t.Errorf("duplicate allocated id %q", id)
		}
		seen[id] = true
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.ListTasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != writers || len(seen) != writers {
		t.Fatalf("tasks=%d unique ids=%d, want %d", len(tasks), len(seen), writers)
	}
	info, err := db.Tag("main")
	if err != nil {
		t.Fatal(err)
	}
	if info.NextID != writers+1 {
		t.Errorf("nextId = %d, want %d", info.NextID, writers+1)
	}
}

func TestConcurrentProcessCASHasExactlyOneWinner(t *testing.T) {
	db, path := newDB(t)
	id := addTask(t, db, "main", "contended")
	db.Close()

	const contenders = 8
	type result struct {
		code int
	}
	results := make(chan result, contenders)
	for i := 0; i < contenders; i++ {
		go func(i int) {
			_, _, code := spawnHelper(t, "cas", path, id, "1", fmt.Sprintf("contender-%d", i))
			results <- result{code}
		}(i)
	}
	winners, losers := 0, 0
	for i := 0; i < contenders; i++ {
		r := <-results
		switch r.code {
		case 0:
			winners++
		case 3:
			losers++
		default:
			t.Errorf("unexpected helper exit code %d", r.code)
		}
	}
	if winners != 1 || losers != contenders-1 {
		t.Fatalf("winners=%d losers=%d, want 1 and %d", winners, losers, contenders-1)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Revision != 2 {
		t.Errorf("revision = %d, want 2", task.Revision)
	}
	if strings.Count(task.Details, "touched by") != 1 {
		t.Errorf("details = %q, want exactly one winner's write", task.Details)
	}
}

func TestConcurrentProcessClaimsHaveExactlyOneWinner(t *testing.T) {
	db, path := newDB(t)
	id := addTask(t, db, "main", "claimable")
	db.Close()

	const contenders = 6
	results := make(chan int, contenders)
	for i := 0; i < contenders; i++ {
		go func(i int) {
			_, _, code := spawnHelper(t, "claim", path, id, fmt.Sprintf("owner-%d", i), fmt.Sprintf("req-%d", i))
			results <- code
		}(i)
	}
	winners := 0
	for i := 0; i < contenders; i++ {
		switch code := <-results; code {
		case 0:
			winners++
		case 3:
		default:
			t.Errorf("unexpected helper exit code %d", code)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want 1", winners)
	}
}

// TestConcurrentProcessClaimNextDistributesUniqueTasks pins §8.3: selection
// and acquisition are one immediate transaction, so concurrent claim-next
// callers each receive a distinct task and none receives a task another
// owner holds; excess callers get the successful empty result.
func TestConcurrentProcessClaimNextDistributesUniqueTasks(t *testing.T) {
	db, path := newDB(t)
	for i := range 3 {
		addTask(t, db, "main", fmt.Sprintf("work-%d", i))
	}
	db.Close()

	const contenders = 8
	type result struct {
		stdout string
		code   int
	}
	results := make(chan result, contenders)
	for i := range contenders {
		go func(i int) {
			stdout, _, code := spawnHelper(t, "claim-next", path, fmt.Sprintf("owner-%d", i), fmt.Sprintf("req-%d", i))
			results <- result{stdout, code}
		}(i)
	}
	claimed := make(map[string]int64, 3)
	tokens := make(map[int64]bool, 3)
	empties := 0
	for range contenders {
		r := <-results
		if r.code != 0 {
			t.Fatalf("contender exit %d", r.code)
		}
		fields := strings.Fields(r.stdout)
		if len(fields) == 1 && fields[0] == "empty" {
			empties++
			continue
		}
		if len(fields) != 2 {
			t.Fatalf("claim-next output %q has %d fields, want task and token", r.stdout, len(fields))
		}
		id := fields[0]
		token, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || token <= 0 {
			t.Fatalf("claim-next token %q is invalid: %v", fields[1], err)
		}
		if _, exists := claimed[id]; exists {
			t.Fatalf("task %s claimed twice", id)
		}
		if tokens[token] {
			t.Fatalf("fencing token %d returned twice", token)
		}
		claimed[id] = token
		tokens[token] = true
	}
	if len(claimed) != 3 || empties != contenders-3 {
		t.Fatalf("claimed=%d empties=%d, want 3 and %d", len(claimed), empties, contenders-3)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	tasks, err := reopened.ListTasks("main")
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, task := range tasks {
		if task.Claim == nil {
			continue
		}
		live++
		if token, ok := claimed[task.ID]; !ok || token != task.Claim.Token {
			t.Fatalf("durable claim for task %s does not match process result", task.ID)
		}
	}
	if live != 3 {
		t.Fatalf("durable live claims = %d, want 3", live)
	}
}

func TestClaimSurvivesRestart(t *testing.T) {
	db, path := newDB(t)
	id := addTask(t, db, "main", "restartable")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-restart-1")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen: the claim and the fencing counter persist across restart.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Claim == nil || task.Claim.Token != claim.Token || task.Claim.Owner != "agent-a" {
		t.Fatalf("claim after restart = %+v", task.Claim)
	}
	// Renew by token across the restart draws a fresh fencing token.
	renewed, _, err := db.RenewClaim("main", id, "agent-a", claim.Token, 300)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Token <= claim.Token {
		t.Errorf("renewed token = %d, want > %d", renewed.Token, claim.Token)
	}
	// Fencing counter never goes backwards across restarts.
	if fencing, err := db.Fencing(); err != nil || fencing != renewed.Token {
		t.Errorf("fencing = %d, want %d", fencing, renewed.Token)
	}
	// Expiry across restart: force the expiry into the past, reopen, and a
	// new owner acquires without force.
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id=?`, id); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, _, err := db.AcquireClaim("main", id, "agent-b", 300, "req-restart-2"); err != nil {
		t.Fatalf("acquire after expiry across restart: %v", err)
	}
}
