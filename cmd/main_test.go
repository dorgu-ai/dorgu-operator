package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestSetupSignalHandler_CalledOnce(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("failed to read main.go: %v", err)
	}

	source := string(data)

	// Match actual calls like ctrl.SetupSignalHandler() but not comments.
	// Remove all comment lines first.
	codeLines := make([]string, 0, strings.Count(source, "\n")+1)
	for line := range strings.SplitSeq(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		codeLines = append(codeLines, line)
	}
	code := strings.Join(codeLines, "\n")

	re := regexp.MustCompile(`ctrl\.SetupSignalHandler\(\)`)
	matches := re.FindAllString(code, -1)

	if len(matches) != 1 {
		t.Errorf("ctrl.SetupSignalHandler() must be called exactly once in main.go, found %d calls", len(matches))
	}
}
