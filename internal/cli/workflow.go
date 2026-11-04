package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const workflowUsage = "usage: co workflow check [FILE]"
const checkerUnavailable = "workflow checker unavailable; install @cocodes/workflows in this repository and run from its root"

func runWorkflow(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 1 || len(args) > 2 || args[0] != "check" || len(args) == 2 && strings.HasPrefix(args[1], "-") {
		return errors.New(workflowUsage)
	}
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("unable to read current directory: %s", processError(err))
	}
	checker := filepath.Join(root, "node_modules", "@cocodes", "workflows", "src", "check.ts")
	info, err := os.Stat(checker)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New(checkerUnavailable)
	}
	arguments := []string{"run", checker}
	if len(args) == 2 {
		arguments = append(arguments, args[1])
	}
	bun, err := lookupBun()
	if err != nil {
		return fmt.Errorf("unable to launch Bun workflow checker: %s", processError(err))
	}
	// Resolve explicitly so relative PATH entries retain Rust's execvp behavior
	// without Go's ErrDot rejection or changing the script's invoked path.
	child := &exec.Cmd{Path: bun, Args: append([]string{"bun"}, arguments...)}
	child.Dir, child.Stdin, child.Stdout, child.Stderr = root, stdin, stdout, stderr
	if err := child.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("workflow check failed (%s)", processStatus(exit.ProcessState))
		}
		return fmt.Errorf("unable to launch Bun workflow checker: %s", processError(err))
	}
	return nil
}
