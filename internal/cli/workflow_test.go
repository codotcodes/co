package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestWorkflowArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"unknown"}, {"check", "-h"}, {"check", "--"}, {"check", "file", "extra"}} {
		if err := Run(append([]string{"workflow"}, args...), strings.NewReader(""), io.Discard, io.Discard); err == nil || err.Error() != workflowUsage {
			t.Fatalf("args %q: %v", args, err)
		}
	}
}

func TestPartialRuntimeNeverFallsBack(t *testing.T) {
	if err := Run([]string{"machine", "run", "fixture"}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "not migrated to Go") {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &output, io.Discard); err != nil || !strings.Contains(output.String(), "partial Go migration") {
		t.Fatal(output.String(), err)
	}
}
