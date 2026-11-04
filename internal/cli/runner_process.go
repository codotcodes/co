package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const runnerStepTimeout = 600 * time.Second
const runnerDefaultImage = "docker.io/library/alpine@sha256:1f3591b8a02ea153f41c5bba878ad477f63ab3d19349762cb77504db02a23e15"

func runnerProcess(name string, args []string) (*exec.Cmd, error) {
	command, err := repositoryProcess(name, args)
	if err != nil {
		return nil, err
	}
	isolateRunnerProcess(command)
	return command, nil
}

func runnerCheckout(ctx context.Context, job runnerAssignment, root, api string) error {
	if !validRunnerAssignment(job, api) {
		return errors.New("invalid job assignment")
	}
	for index, args := range [][]string{{"clone", "--no-checkout", "--", job.CheckoutURL, "source"}, {"-C", "source", "checkout", "--detach", job.CommitOID}} {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		child, err := runnerProcess("git", args)
		if err != nil {
			if index == 0 {
				return fmt.Errorf("unable to start checkout: %s", processError(err))
			}
			return fmt.Errorf("unable to check out commit: %s", processError(err))
		}
		child.Dir = root
		environment := map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull}
		if index == 0 {
			environment["GIT_TERMINAL_PROMPT"] = "0"
			environment["GIT_CONFIG_COUNT"] = "3"
			environment["GIT_CONFIG_KEY_0"] = "http.extraHeader"
			environment["GIT_CONFIG_VALUE_0"] = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("co:"+job.Token))
			environment["GIT_CONFIG_KEY_1"], environment["GIT_CONFIG_VALUE_1"] = "http.followRedirects", "false"
			environment["GIT_CONFIG_KEY_2"], environment["GIT_CONFIG_VALUE_2"] = "credential.helper", ""
		}
		child.Env = runnerEnvironment(environment)
		if err := child.Start(); err != nil {
			if index == 0 {
				return fmt.Errorf("unable to start checkout: %s", processError(err))
			}
			return fmt.Errorf("unable to check out commit: %s", processError(err))
		}
		bounded, cancel := context.WithTimeout(ctx, runnerStepTimeout)
		code, completed := waitRunnerProcess(bounded, child)
		cancel()
		if !completed || code != 0 {
			if index == 0 {
				return errors.New("unable to fetch the exact commit")
			}
			return errors.New("exact commit is unavailable")
		}
	}
	return nil
}

func runnerEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if _, replaced := overrides[key]; !replaced {
			environment = append(environment, value)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func writeRunnerEnvironment(directory string, environment map[string]string) (string, error) {
	if !validRunnerEnvironment(environment) {
		return "", errors.New("invalid environment")
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var script strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&script, "export %s='%s'\n", key, strings.ReplaceAll(environment[key], "'", "'\\''"))
	}
	path := filepath.Join(directory, "environment.sh")
	if err := os.WriteFile(path, []byte(script.String()), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func writeRunnerActionInputs(directory string, files []runnerActionFile) error {
	if len(files) > 4096 {
		return errors.New("too many action files")
	}
	total := 0
	for _, file := range files {
		if !validArtifactPath(file.Path) || strings.ContainsAny(file.Content, "\r\n") {
			return errors.New("invalid action path or encoding")
		}
		data, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return errors.New("invalid action encoding")
		}
		total += len(data)
		if total > runnerArtifactLimit || runnerDigest(data) != file.SHA256 {
			return errors.New("action content verification failed")
		}
		path := filepath.Join(directory, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = output.Write(data)
		closeErr := output.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func rootlessRunnerOutbound(ctx context.Context) bool {
	for index, probe := range [][]string{{"podman", "info", "--format", "{{.Host.Security.Rootless}}"}, {"slirp4netns", "--version"}} {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		command := exec.CommandContext(bounded, probe[0], probe[1:]...)
		data, err := command.Output()
		cancel()
		if err != nil || index == 0 && strings.TrimSpace(string(data)) != "true" {
			return false
		}
	}
	return true
}

func executeRunnerStep(ctx context.Context, image, checkout string, step runnerStep, job runnerAssignment, logs *runnerLogs, runtimeRoot, cacheRoot string) runnerStepResult {
	index := logs.begin(step.Name)
	fail := func(message string) runnerStepResult {
		logs.append(index, message)
		return runnerStepResult{Name: step.Name, ExitCode: 1, Output: logs.finish(index, 1)}
	}
	if ctx.Err() != nil {
		return fail("Workflow cancelled.")
	}
	network, valid := runnerStepNetwork(job, step)
	if !valid || network != "none" && !rootlessRunnerOutbound(ctx) {
		return fail("Outbound actions require rootless networking and an explicit action environment.")
	}
	workspace, err := os.MkdirTemp("", "co-step-")
	if err != nil {
		return fail("Unable to prepare the workflow environment.")
	}
	defer os.RemoveAll(workspace)
	environment := job.Environment
	if step.ActionEnvironment != nil {
		environment = *step.ActionEnvironment
	} else if !validRunnerSecrets(job) {
		return fail("Unable to prepare the workflow environment.")
	}
	environmentFile, err := writeRunnerEnvironment(workspace, environment)
	if err != nil {
		return fail("Unable to prepare the workflow environment.")
	}
	inputs := filepath.Join(workspace, "inputs")
	if err := os.Mkdir(inputs, 0700); err != nil {
		return fail("Unable to prepare verified action inputs.")
	}
	if err := writeRunnerActionInputs(inputs, step.ActionFiles); err != nil {
		return fail("Unable to prepare verified action inputs.")
	}
	container := fmt.Sprintf("co-%s-%d", job.ID, index)
	args := []string{"run", "--rm", "--pull=never", "--name", container, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--userns=keep-id", "--tmpfs=/tmp:rw,nosuid,nodev,size=64m", "--workdir=/workspace", "--env=HOME=/tmp", "--env=PATH=/usr/local/bin:/usr/bin:/bin", "--network=" + network, fmt.Sprintf("--memory=%dm", job.RAMMiB), "--label=io.co.codes.machine=" + job.MachineID, fmt.Sprintf("--memory-swap=%dm", job.RAMMiB), fmt.Sprintf("--cpus=%d", job.VCPUs), "--volume=" + environmentFile + ":/run/co-environment.sh:ro,Z"}
	if len(step.ActionFiles) > 0 {
		args = append(args, "--volume="+inputs+":/inputs:ro,Z")
	}
	outputs := ""
	if step.ActionOutputs != nil {
		outputs = filepath.Join(workspace, "outputs")
		if err := os.Mkdir(outputs, 0700); err != nil {
			outputs = ""
		} else {
			args = append(args, "--volume="+outputs+":/run/co-action-output:rw,Z")
		}
	}
	args = append(args, "--volume="+checkout+":/workspace:rw,Z")
	if step.GitHub {
		if runtimeRoot == "" {
			return fail("Unable to prepare the GitHub-compatible job runtime.")
		}
		if cacheRoot != "" {
			args = append(args, "--volume="+cacheRoot+":/run/co-cache:ro,z")
		}
		args = append(args, "--volume="+runtimeRoot+":/run/co-job:rw,Z")
		for _, asset := range []struct{ source, target string }{{"command_files.mjs", "command_files.mjs"}, {"cache_runtime.mjs", "cache_runtime.mjs"}, {"action_runtime.mjs", "co-action.mjs"}} {
			data, err := runnerRuntime.ReadFile("runtime/" + asset.source)
			path := filepath.Join(workspace, asset.source)
			if err != nil || os.WriteFile(path, data, 0600) != nil {
				return fail("Unable to prepare the GitHub-compatible job runtime.")
			}
			args = append(args, "--volume="+path+":/run/"+asset.target+":ro,Z")
		}
		shell := "bash"
		if step.Shell != nil {
			shell = *step.Shell
		}
		payload, err := json.Marshal(map[string]any{"command": step.Run, "shell": shell, "id": step.ID, "outputKeys": step.ActionOutputs, "env": map[string]string{"GITHUB_SHA": job.CommitOID}})
		payloadPath := filepath.Join(workspace, "step.json")
		if err != nil || os.WriteFile(payloadPath, payload, 0600) != nil {
			return fail("Unable to prepare the GitHub-compatible job runtime.")
		}
		args = append(args, "--volume="+payloadPath+":/run/co-step.json:ro,Z")
	}
	script := ". /run/co-environment.sh\n"
	if outputs != "" {
		script += "export CO_ACTION_OUTPUT=/run/co-action-output/outputs.json\n"
	}
	if step.GitHub {
		script += "exec /usr/local/bin/node /run/command_files.mjs /run/co-step.json"
	} else {
		script += "exec /bin/sh -e -c \"$1\""
	}
	args = append(args, image, "/bin/sh", "-e", "-c", script, "co-step", step.Run)
	child, err := runnerProcess("podman", args)
	if err != nil {
		return fail("Unable to start container: " + processError(err))
	}
	stdout, outWrite, err := os.Pipe()
	if err != nil {
		return fail("Unable to start container: " + processError(err))
	}
	defer stdout.Close()
	stderr, errWrite, err := os.Pipe()
	if err != nil {
		outWrite.Close()
		return fail("Unable to start container: " + processError(err))
	}
	defer stderr.Close()
	child.Stdout, child.Stderr = outWrite, errWrite
	if err := child.Start(); err != nil {
		outWrite.Close()
		errWrite.Close()
		return fail("Unable to start container: " + processError(err))
	}
	outWrite.Close()
	errWrite.Close()
	var readers sync.WaitGroup
	readers.Add(2)
	for _, stream := range []io.Reader{stdout, stderr} {
		go func() { defer readers.Done(); drainRunnerLogs(stream, logs, index, job.SecretValues) }()
	}
	bounded, cancel := context.WithTimeout(ctx, runnerStepTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	code, completed := waitRunnerProcess(bounded, child)
	stopContainer := func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_ = exec.CommandContext(stopCtx, "podman", "stop", "--time=0", container).Run()
		stop()
	}
	if !completed {
		stopContainer()
	}
	select {
	case <-done:
	case <-bounded.Done():
		if completed {
			stopContainer()
		}
		stdout.Close()
		stderr.Close()
		<-done
		code = 1
	}
	result := runnerStepResult{Name: step.Name, ExitCode: code, Output: logs.finish(index, code)}
	if code == 0 && outputs != "" {
		result.Outputs = collectRunnerActionOutputs(outputs, *step.ActionOutputs, job.SecretValues)
	}
	return result
}
