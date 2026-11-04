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

func TestRuntimeNeverFallsBack(t *testing.T) {
	if err := Run([]string{"unknown-command"}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &output, io.Discard); err != nil || !strings.Contains(output.String(), "the co.codes command line") || strings.Contains(output.String(), "Rust") {
		t.Fatal(output.String(), err)
	}
}
