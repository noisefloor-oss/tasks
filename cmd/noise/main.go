package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/buildinfo"
	"github.com/noisefloor-oss/tasks/internal/libexec"
)

const shortUsage = "usage: noise <noun> <command> [args...]"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return 0
	}

	word := args[0]
	switch word {
	case "help", "--help", "-h":
		if len(args) != 1 {
			return usageError(stderr, "%s does not accept arguments", word)
		}
		printHelp(stdout)
		return 0
	case "version", "--version":
		if len(args) != 1 {
			return usageError(stderr, "%s does not accept arguments", word)
		}
		fmt.Fprintf(stdout, "noise %s\n", buildinfo.Version)
		return 0
	case "plugins":
		if len(args) != 1 {
			return usageError(stderr, "plugins does not accept arguments")
		}
		if code := requireSupported(stderr); code != 0 {
			return code
		}
		nouns, err := libexec.Discover()
		if err != nil {
			return rootError(stderr, err)
		}
		for _, noun := range nouns {
			fmt.Fprintln(stdout, noun)
		}
		return 0
	}

	if !libexec.ValidNoun(word) {
		return usageError(stderr, "invalid plugin noun: %s", word)
	}
	if code := requireSupported(stderr); code != 0 {
		return code
	}
	target, err := libexec.Resolve(word)
	if err != nil {
		switch {
		case errors.Is(err, libexec.ErrInstallation):
			return rootError(stderr, err)
		case errors.Is(err, libexec.ErrNotInstalled):
			fmt.Fprintf(stderr, "noise: plugin not installed: %s\n", word)
			return 127
		default:
			fmt.Fprintf(stderr, "noise: unsafe plugin: %s: %v\n", word, err)
			return 126
		}
	}
	env, err := libexec.ProjectEnvironment(os.Environ())
	if err != nil {
		fmt.Fprintf(stderr, "noise: project context: %v\n", err)
		return 1
	}
	argv := append([]string{"noise-" + word}, args[1:]...)
	if err := libexec.Exec(target, argv, env); err != nil {
		fmt.Fprintf(stderr, "noise: execute %s: %v\n", word, err)
		return 126
	}
	return 0
}

func requireSupported(stderr io.Writer) int {
	if libexec.Supported() {
		return 0
	}
	fmt.Fprintf(stderr, "noise: unsupported platform: %s\n", libexec.Platform)
	return 1
}

func rootError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "noise: installation error at %s: %v\n", libexec.Root, err)
	return 1
}

func usageError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "noise: "+format+"\n", args...)
	fmt.Fprintln(stderr, shortUsage)
	return 2
}

func printHelp(stdout io.Writer) {
	fmt.Fprintln(stdout, strings.TrimSpace(`usage: noise <noun> <command> [args...]

host commands:
  help       show this help
  version    print the host version
  plugins    list installed plugin nouns

plugin help: noise <noun> --help`))
}
