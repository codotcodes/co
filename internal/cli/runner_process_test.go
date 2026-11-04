package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const fixturePodman = `#!/usr/bin/env python3
import hashlib,json,os,pathlib,subprocess,sys,time
args=sys.argv[1:]
if args[0]=='rm':
 if os.environ.get('CO_STEP_CLEAN'):
  with open(os.environ['CO_STEP_CLEAN'],'a') as output:output.write(json.dumps(args)+'\n')
 sys.exit(0)
if args[0]=='image':
 print('1' if os.environ['CO_STEP_CASE']=='cache' else '0');sys.exit(0)
if args[0]=='info':
 print('false' if os.environ['CO_STEP_CASE']=='no-network' else 'true');sys.exit(0)
if args[0]=='stop':
 pathlib.Path(os.environ['CO_STEP_STOP']).write_text('stopped');sys.exit(0)
volumes={}
for arg in args:
 if arg.startswith('--volume='):
  host,guest,options=arg[len('--volume='):].rsplit(':',2);volumes[guest]=(host,options)
environment=pathlib.Path(volumes['/run/co-environment.sh'][0]).read_text()
private=os.environ.get('CO_STEP_PRIVATE','')
assert not private or all(private not in arg for arg in args)
assert not private or os.environ.get('TOKEN')!=private
assert not private or private in environment
required=['--read-only','--cap-drop=ALL','--security-opt=no-new-privileges','--pids-limit=256','--userns=keep-id','--memory=256m','--memory-swap=256m','--cpus=1','--label=io.co.codes.machine=machine']
assert all(value in args for value in required)
if '/inputs' in volumes:
 assert pathlib.Path(volumes['/inputs'][0],'src','input').read_text()=='verified input'
if '/run/co-step.json' in volumes:
 payload=json.loads(pathlib.Path(volumes['/run/co-step.json'][0]).read_text())
 assert payload['env']['GITHUB_SHA']=='a'*40 and payload['shell']=='bash'
 for guest in ['/run/command_files.mjs','/run/co-action.mjs','/run/cache_runtime.mjs']:
  assert pathlib.Path(volumes[guest][0]).stat().st_size>100
if '/run/co-action-output' in volumes:
 pathlib.Path(volumes['/run/co-action-output'][0],'outputs.json').write_text(json.dumps({'url':'https://site.invalid','version_id':private}))
pathlib.Path(os.environ['CO_STEP_CALLED']).write_text('called')
if os.environ['CO_STEP_CASE']=='cancel':
 time.sleep(10)
sys.stdout.write('before sec');sys.stdout.flush();time.sleep(0.01)
sys.stdout.write('ret after\n');sys.stdout.flush()
sys.exit(7 if os.environ['CO_STEP_CASE']=='failure' else 0)
`

func TestRunnerStepMatchesCompiledRust(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process/container-launch fixtures")
	}
	binary := os.Getenv("CO_TEST_RUST_RUNNER_STEP_BINARY")
	if binary == "" {
		t.Skip("set the frozen retained Rust step probe")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"normal", "failure", "github", "inputs", "outputs", "bad-input", "no-network", "outbound"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			writeFixture(t, filepath.Join(bin, "podman"), strings.Replace(fixturePodman, "#!/usr/bin/env python3", "#!"+python, 1), 0500)
			writeFixture(t, filepath.Join(bin, "slirp4netns"), "#!/bin/sh\nexit 0\n", 0500)
			called := filepath.Join(root, "called")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMPDIR", root)
			t.Setenv("CO_STEP_CASE", scenario)
			t.Setenv("CO_STEP_CALLED", called)
			t.Setenv("CO_STEP_STOP", filepath.Join(root, "stopped"))
			t.Setenv("CO_STEP_PRIVATE", "secret")
			checkout, runtimeRoot := filepath.Join(root, "checkout"), filepath.Join(root, "runtime")
			if err := os.Mkdir(checkout, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(runtimeRoot, 0700); err != nil {
				t.Fatal(err)
			}
			step := map[string]any{"name": "build", "run": "echo fixture", "actionEnvironment": map[string]string{"TOKEN": "secret"}}
			switch scenario {
			case "github":
				step["github"] = true
			case "inputs", "bad-input":
				data := []byte("verified input")
				hash := runnerDigest(data)
				if scenario == "bad-input" {
					hash = strings.Repeat("0", 64)
				}
				step["actionFiles"] = []any{map[string]any{"path": "src/input", "content": base64.StdEncoding.EncodeToString(data), "sha256": hash}}
			case "outputs":
				step["actionOutputs"] = []string{"url", "version_id"}
			case "no-network", "outbound":
				step["actionNetwork"] = "outbound"
			}
			jobData := map[string]any{"id": "job", "assignmentToken": "assignment", "checkoutUrl": "https://git.co.codes/owner/repo.git", "commitOid": strings.Repeat("a", 40), "scope": "owner", "steps": []any{step}, "vcpus": 1, "ramMiB": 256, "environment": map[string]string{}, "secretValues": []string{"secret"}}
			raw, _ := json.Marshal(jobData)
			job, err := decodeRunnerAssignment(raw)
			if err != nil {
				t.Fatal(err)
			}
			job.MachineID = "machine"
			input, _ := json.Marshal(map[string]any{"mode": "step", "job": jobData, "image": "fixture@sha256:pinned", "checkout": checkout, "runtime": runtimeRoot})
			command := exec.CommandContext(t.Context(), binary, "__migration-validate")
			command.Stdin = bytes.NewReader(append(input, '\n'))
			out, err := command.Output()
			if err != nil {
				t.Fatal("Rust step probe failed", err)
			}
			var expected runnerStepResult
			if err := json.Unmarshal(out, &expected); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(called)
			logs := &runnerLogs{}
			actual := executeRunnerStep(context.Background(), "fixture@sha256:pinned", checkout, job.Steps[0], job, logs, runtimeRoot, "")
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("step differs: Go=%+v Rust=%+v", actual, expected)
			}
			if strings.Contains(actual.Output, "secret") {
				t.Fatal("private value reached step output")
			}
			if scenario == "bad-input" || scenario == "no-network" {
				if _, err := os.Stat(called); !os.IsNotExist(err) {
					t.Fatal("unsafe step reached container launch")
				}
			}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "co-step-") {
					t.Fatal("step left private temporary inputs behind")
				}
			}
		})
	}
	t.Log("8 compiled retained-Rust/Go step executions match outputs/status/declared outputs using task-owned Podman fixtures; host argv/environment and sandbox flags checked")
}

func TestRunnerStepCancellationStopsContainerAndDrains(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux cancellation fixture")
	}
	root := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	writeFixture(t, filepath.Join(bin, "podman"), strings.Replace(fixturePodman, "#!/usr/bin/env python3", "#!"+python, 1), 0500)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", root)
	t.Setenv("CO_STEP_CASE", "cancel")
	t.Setenv("CO_STEP_CALLED", filepath.Join(root, "called"))
	t.Setenv("CO_STEP_STOP", filepath.Join(root, "stopped"))
	t.Setenv("CO_STEP_PRIVATE", "secret")
	job := runnerAssignment{ID: "job", MachineID: "machine", Token: "assignment", VCPUs: 1, RAMMiB: 256, Environment: map[string]string{"TOKEN": "secret"}, SecretValues: []string{"secret"}}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	result := executeRunnerStep(ctx, "image@sha256:pinned", root, runnerStep{Name: "build", Run: "fixture"}, job, &runnerLogs{}, "", "")
	if result.ExitCode != 1 {
		t.Fatal("cancelled container reported success")
	}
	if _, err := os.Stat(filepath.Join(root, "stopped")); err != nil {
		t.Fatal("cancelled step did not stop named container", err)
	}
}
