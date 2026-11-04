package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type observation struct {
	Code           int
	Stdout, Stderr string
}

func invoke(t *testing.T, binary, root string, args, extraEnv []string) observation {
	return invokeInput(t, binary, root, args, extraEnv, []byte("unchanged stdin\n"))
}

func invokeInput(t *testing.T, binary, root string, args, extraEnv []string, input []byte) observation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = root
	command.Env = append([]string{"HOME=" + root, "CO_CONFIG_DIR=" + filepath.Join(root, "must-not-load"), "CO_API_URL=http://127.0.0.1:1", "PATH=" + os.Getenv("PATH")}, extraEnv...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatal("CLI exceeded test deadline", ctx.Err())
	}
	code := 0
	if err != nil {
		var ok bool
		var exit *exec.ExitError
		if exit, ok = err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return observation{code, stdout.String(), stderr.String()}
}

func comparisonBinaries(t *testing.T) (string, string) {
	t.Helper()
	rust := os.Getenv("CO_TEST_RUST_BINARY")
	if rust == "" {
		t.Skip("set a frozen CO_TEST_RUST_BINARY for migration comparisons")
	}
	return rust, goBinaryForTests(t)
}

func compare(t *testing.T, rust, goBinary, root string, args, env []string) observation {
	t.Helper()
	want := invoke(t, rust, root, args, env)
	actual := invoke(t, goBinary, root, args, env)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("%q: Go=%+v Rust=%+v", args, actual, want)
	}
	return actual
}

func checkerPath(root string) string {
	return filepath.Join(root, "node_modules", "@cocodes", "workflows", "src", "check.ts")
}
func writeFixture(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledWorkflowRustParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixtures")
	}
	rust, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "must-not-load", "config.json"), "not valid JSON", 0600)
	count := 0
	for _, args := range [][]string{{"workflow"}, {"workflow", "--help"}, {"workflow", "check", "-h"}, {"workflow", "other"}, {"workflow", "check", "--"}, {"workflow", "check", "a", "b"}, {"workflow", "check"}, {"workflow", "check", "file.ts"}} {
		result := compare(t, rust, goBinary, root, args, nil)
		if result.Code != 1 {
			t.Fatal(result)
		}
		count++
	}
	if err := os.MkdirAll(checkerPath(root), 0700); err != nil {
		t.Fatal(err)
	}
	compare(t, rust, goBinary, root, []string{"workflow", "check"}, nil)
	count++
	if err := os.Remove(checkerPath(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, checkerPath(root), "throw new Error('controlled checker fixture');\n", 0600)
	for _, mode := range []string{"success", "failure", "signal", "missing", "permission", "relative-path", "current-path", "dot-path", "permission-fallback"} {
		t.Run(mode, func(t *testing.T) {
			bin := filepath.Join(root, "bin-"+mode)
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			body := "#!/bin/sh\nprintf 'cwd=%s\\n' \"$PWD\"\nprintf 'invoked=%s\\n' \"$0\"\nprintf 'arg=%s\\n' \"$@\"\nIFS= read -r line\nprintf '%s\\n' \"$line\"\nprintf 'checker diagnostic\\n' >&2\n"
			if mode == "failure" {
				body += "exit 7\n"
			}
			if mode == "signal" {
				body += "kill -TERM $$\n"
			}
			executable := filepath.Join(bin, "bun")
			if mode == "current-path" || mode == "dot-path" {
				executable = filepath.Join(root, "bun")
			}
			if mode != "missing" {
				permissions := os.FileMode(0500)
				if mode == "permission" {
					permissions = 0400
				}
				writeFixture(t, executable, body, permissions)
			}
			path := bin
			if mode == "relative-path" {
				path = "bin-" + mode
			}
			if mode == "current-path" {
				path = ""
			}
			if mode == "dot-path" {
				path = "."
			}
			if mode == "permission-fallback" {
				path = filepath.Join(root, "bin-permission") + ":" + bin
			}
			for _, args := range [][]string{{"workflow", "check"}, {"workflow", "check", ".co/workflows/space + 日本語.ts"}, {"workflow", "check", ""}} {
				result := compare(t, rust, goBinary, root, args, []string{"PATH=" + path})
				if (mode == "success" || mode == "relative-path" || mode == "current-path" || mode == "dot-path" || mode == "permission-fallback") != (result.Code == 0) {
					t.Fatal(result)
				}
				count++
			}
		})
	}
	for _, version := range []string{"version", "--version", "-V"} {
		compare(t, rust, goBinary, root, []string{version}, nil)
		count++
	}
	if !t.Failed() {
		t.Logf("%d compiled workflow/version comparisons match Rust exactly", count)
	}
}

func TestCompiledWorkflowActualBunChecker(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	packageRoot := os.Getenv("CO_TEST_WORKFLOWS_PACKAGE")
	if packageRoot == "" {
		t.Skip("set actual @cocodes/workflows package path")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@cocodes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(packageRoot, filepath.Join(root, "node_modules", "@cocodes", "workflows")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".github", "workflows", "verify.yml")
	valid := "name: Verify\non: push\njobs:\n  verify:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo fixture\n"
	writeFixture(t, path, valid, 0600)
	for _, args := range [][]string{{"workflow", "check"}, {"workflow", "check", ".github/workflows/verify.yml"}} {
		result := compare(t, rust, goBinary, root, args, nil)
		if result.Code != 0 {
			t.Fatalf("valid actual checker failed: %+v", result)
		}
	}
	writeFixture(t, path, "jobs: [broken\n", 0600)
	result := compare(t, rust, goBinary, root, []string{"workflow", "check"}, nil)
	if result.Code != 1 || !strings.Contains(result.Stderr, "workflow check failed") {
		t.Fatalf("invalid workflow accepted: %+v", result)
	}
	marker := filepath.Join(root, "evaluated")
	writeFixture(t, filepath.Join(root, ".co", "workflows", "unsafe.ts"), "await Bun.write("+"\""+marker+"\""+", 'unsafe');\nthrow new Error('must not evaluate');\n", 0600)
	compare(t, rust, goBinary, root, []string{"workflow", "check", ".co/workflows/unsafe.ts"}, nil)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("checker evaluated repository code", err)
	}
	t.Log("4 actual Bun checker comparisons match Rust; valid/invalid selection and repository code is not evaluated")
}
