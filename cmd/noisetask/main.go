package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/noisefloor-oss/tasks/internal/libexec"
)

const warning = "warning: noisetask is deprecated; use noise tasks (or noise-tasks); removal is gated by the published migration plan"

func main() {
	fmt.Fprintln(os.Stderr, warning)
	if !libexec.Supported() {
		fmt.Fprintf(os.Stderr, "noisetask: unsupported platform: %s\n", libexec.Platform)
		os.Exit(1)
	}
	target, err := libexec.Resolve("tasks")
	if err != nil {
		switch {
		case errors.Is(err, libexec.ErrInstallation):
			fmt.Fprintf(os.Stderr, "noisetask: installation error at %s: %v\n", libexec.Root, err)
			os.Exit(1)
		case errors.Is(err, libexec.ErrNotInstalled):
			fmt.Fprintln(os.Stderr, "noisetask: plugin not installed: tasks")
			os.Exit(127)
		default:
			fmt.Fprintf(os.Stderr, "noisetask: unsafe plugin: tasks: %v\n", err)
			os.Exit(126)
		}
	}
	env, err := libexec.ProjectEnvironment(os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "noisetask: project context: %v\n", err)
		os.Exit(1)
	}
	argv := append([]string{"noise-tasks"}, os.Args[1:]...)
	if err := libexec.Exec(target, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "noisetask: execute noise-tasks: %v\n", err)
		os.Exit(126)
	}
}
