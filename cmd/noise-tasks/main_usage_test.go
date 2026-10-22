package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestTopLevelUsageCommandRowsHaveDescriptionGap(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	inCommands := false
	commandRow := regexp.MustCompile(`^  \S+ {2,}\S`)
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "commands:" {
			inCommands = true
			continue
		}
		if !inCommands || line == "" {
			continue
		}
		if strings.HasPrefix(line, "run '") {
			break
		}
		if !commandRow.MatchString(line) {
			t.Errorf("top-level command row %q must separate command and description with at least two spaces", line)
		}
	}
}
