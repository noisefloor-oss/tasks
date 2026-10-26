package main

import (
	"fmt"
	"io"
	"os"

	"github.com/noisefloor-oss/tasks/internal/buildinfo"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}

	cmd := os.Args[1]
	if cmd == "--help" || cmd == "-help" || cmd == "-h" {
		if len(os.Args) != 2 {
			printUsage(os.Stderr)
			os.Exit(2)
		}
		printUsage(os.Stdout)
		return
	}
	switch cmd {
	case "setup":
		os.Exit(cmdSetup(os.Args[2:], os.Stdout, os.Stderr))
	case "init":
		os.Exit(cmdInit(os.Args[2:], os.Stdout, os.Stderr))
	case "list":
		os.Exit(cmdList(os.Args[2:], os.Stdout, os.Stderr))
	case "next":
		os.Exit(cmdNext(os.Args[2:], os.Stdout, os.Stderr))
	case "show":
		os.Exit(cmdShow(os.Args[2:], os.Stdout, os.Stderr))
	case "add":
		os.Exit(cmdAdd(os.Args[2:], os.Stdout, os.Stderr))
	case "ensure":
		os.Exit(cmdEnsure(os.Args[2:], os.Stdout, os.Stderr))
	case "set-status":
		os.Exit(cmdSetStatus(os.Args[2:], os.Stdout, os.Stderr))
	case "append":
		os.Exit(cmdAppend(os.Args[2:], os.Stdout, os.Stderr))
	case "remove":
		os.Exit(cmdRemove(os.Args[2:], os.Stdout, os.Stderr))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "context":
		os.Exit(cmdContext(os.Args[2:], os.Stdout, os.Stderr))
	case "generate", "parse-prd":
		os.Exit(cmdGenerate(os.Args[2:], os.Stdout, os.Stderr))
	case "claim":
		os.Exit(cmdClaim(os.Args[2:], os.Stdout, os.Stderr))
	case "claim-next":
		os.Exit(cmdClaimNext(os.Args[2:], os.Stdout, os.Stderr))
	case "release":
		os.Exit(cmdRelease(os.Args[2:], os.Stdout, os.Stderr))
	case "live":
		os.Exit(cmdLive(os.Args[2:], os.Stdout, os.Stderr))
	case "hygiene":
		os.Exit(cmdHygiene(os.Args[2:], os.Stdout, os.Stderr))
	case "worktree":
		os.Exit(cmdWorktree(os.Args[2:], os.Stdout, os.Stderr))
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:], os.Stdout, os.Stderr))
	case "milestone":
		os.Exit(cmdMilestone(os.Args[2:], os.Stdout, os.Stderr))
	case "backup":
		os.Exit(cmdBackup(os.Args[2:], os.Stdout, os.Stderr))
	case "migrate":
		os.Exit(cmdMigrate(os.Args[2:], os.Stdout, os.Stderr))
	case "plan":
		os.Exit(cmdPlan(os.Args[2:], os.Stdout, os.Stderr))
	case "run":
		os.Exit(cmdRun(os.Args[2:], os.Stdout, os.Stderr))
	case "dispatch":
		os.Exit(cmdDispatch(os.Args[2:], os.Stdout, os.Stderr))
	case "set-milestone":
		os.Exit(cmdSetMilestone(os.Args[2:], os.Stdout, os.Stderr))
	case "set-link":
		os.Exit(cmdSetLink(os.Args[2:], os.Stdout, os.Stderr))
	case "version":
		os.Exit(cmdVersion(os.Args[2:], os.Stdout, os.Stderr))
	default:
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("version", stderr)
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks version")
		return 2
	}
	fmt.Fprintf(stdout, "noise-tasks %s\n", buildinfo.Version)
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `usage: noise-tasks <command> [options]

commands:
  setup       configure project task generation
  init        create the task database
  list        list tasks in a tag
  next        show the next actionable task
  show        show a single task by id
  add         create a new task
  ensure      create-or-return the task tracking a repository issue
  set-status  set a task's status
  set-milestone  assign or clear a task's milestone
  set-link    assign a task's set-once external link
  append      append text to a task's details
  remove      remove a task
  milestone   milestone registry commands
  hygiene   residue report (stale runs, expired claims) plus unowned worktrees
  backup      backup bundle create, verify, and restore
  migrate     plan and apply the one-way live.json migration; adopt imports a retired layout
  plan        validate and atomically apply orchestration plans
  doctor     workspace inventory of task-family authorities
  run         manage durable orchestrator runs and attempts
  verify      check backlog integrity
  context     report read-only project context as JSON
  generate    generate tasks from a PRD
  parse-prd   deprecated compatibility alias for generate
  claim       acquire a task claim
  claim-next  acquire the next claimable task
  release     drop a task claim
  live        change feed
  worktree    task-bound worktree commands
  dispatch  replayable dispatch preparation and reconciliation
  version     print version

run 'noise-tasks <command> -help' for per-command flags.
`)
}
