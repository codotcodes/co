package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runnerTimings struct {
	lease, renewal, job time.Duration
}

var defaultRunnerTimings = runnerTimings{lease: 70 * time.Second, renewal: 20 * time.Second, job: 1800 * time.Second}

func runnerMachineCredential(machine machineIdentity) *apiCredential {
	return &apiCredential{scheme: "Bearer", secret: machine.Credential, scoped: true}
}

func finishRunnerJob(ctx context.Context, machine machineIdentity, job runnerAssignment, steps []runnerStepResult, success bool) error {
	if !validAgentIdentity(agentIdentity{ID: job.ID, Name: "job"}) || !cleanCredential(job.Token) {
		return errors.New("invalid job assignment")
	}
	if steps == nil {
		steps = []runnerStepResult{}
	}
	tuples := make([][3]any, 0, len(steps))
	for _, step := range steps {
		tuples = append(tuples, [3]any{step.Name, step.ExitCode, step.Output})
	}
	result := "failure"
	if success {
		result = "success"
	}
	response, err := requestAPI(ctx, machine.API, "POST", "/machine-enrollment/jobs/"+job.ID+"/finish", runnerMachineCredential(machine), map[string]any{"assignmentToken": job.Token, "result": result, "steps": steps, "hash": runnerDigest(canonicalRunnerJSON(tuples))})
	if err != nil {
		return err
	}
	if response.StatusCode == 409 {
		response.Body.Close()
		return errors.New("assignment expired or cancelled; result discarded")
	}
	if response.StatusCode == 204 {
		response.Body.Close()
		return nil
	}
	_, err = decodeSessionResponse(response)
	return redactAgentError(err, append([]string{machine.Credential, job.Token}, job.SecretValues...)...)
}

func uploadRunnerArtifact(ctx context.Context, machine machineIdentity, job runnerAssignment, data []byte) error {
	digest := runnerDigest(data)
	response, err := requestAPIReader(ctx, machine.API, "POST", "/machine-enrollment/jobs/"+job.ID+"/artifact", runnerMachineCredential(machine), bytes.NewReader(data), "application/vnd.co.artifact", map[string]string{"X-Co-Assignment": job.Token, "X-Co-Artifact-Sha256": digest})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return errors.New("artifact upload rejected")
	}
	receipt, err := decodeSessionResponse(response)
	if err != nil {
		return errors.New("invalid artifact receipt")
	}
	fields, _ := receipt.(map[string]any)
	if fields["digest"] != digest || jsonUint(fields["size"]) != uint64(len(data)) {
		return errors.New("invalid artifact receipt")
	}
	return nil
}

func monitorRunnerLease(ctx context.Context, machine machineIdentity, job runnerAssignment, requestedAt time.Time, timings runnerTimings, cancelJob context.CancelFunc) func() {
	monitorCtx, stop := context.WithCancel(ctx)
	var workers sync.WaitGroup
	var mu sync.Mutex
	deadline := requestedAt.Add(timings.lease)
	workers.Add(2)
	go func() {
		defer workers.Done()
		for monitorCtx.Err() == nil {
			mu.Lock()
			expired := !time.Now().Before(deadline)
			mu.Unlock()
			if expired {
				cancelJob()
				return
			}
			if waitForDevice(monitorCtx, min(100*time.Millisecond, timings.lease)) != nil {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for waitForDevice(monitorCtx, timings.renewal) == nil {
			started := time.Now()
			response, err := requestAPI(monitorCtx, machine.API, "POST", "/machine-enrollment/jobs/"+job.ID+"/renew", runnerMachineCredential(machine), map[string]string{"assignmentToken": job.Token})
			if err != nil {
				continue
			}
			status := response.StatusCode
			response.Body.Close()
			if status >= 200 && status < 300 {
				mu.Lock()
				deadline = started.Add(timings.lease)
				mu.Unlock()
			}
			if status == 401 || status == 409 {
				cancelJob()
				return
			}
		}
	}()
	return func() { stop(); workers.Wait() }
}

func executeRunnerJob(ctx context.Context, machine machineIdentity, job runnerAssignment, requestedAt time.Time, timings runnerTimings, logs *runnerLogs) ([]runnerStepResult, bool, []runnerCachePublication) {
	jobCtx, cancel := context.WithTimeout(ctx, timings.job)
	defer cancel()
	stopMonitor := monitorRunnerLease(jobCtx, machine, job, requestedAt, timings, cancel)
	defer stopMonitor()
	var steps []runnerStepResult
	var publications []runnerCachePublication
	artifactFailed := false
	run := func() error {
		root, err := os.MkdirTemp("", "co-job-")
		if err != nil {
			return errors.New("unable to create workspace")
		}
		defer os.RemoveAll(root)
		if err := runnerCheckout(jobCtx, job, root, machine.API); err != nil {
			return err
		}
		image := runnerDefaultImage
		if selected, present := os.LookupEnv("CO_MACHINE_IMAGE"); present {
			image = selected
		}
		if !containsRunnerImageDigest(image) {
			return errors.New("CO_MACHINE_IMAGE must be pinned by digest")
		}
		runtimeRoot, err := os.MkdirTemp("", "co-runtime-")
		if err != nil {
			return errors.New("unable to create job runtime")
		}
		defer os.RemoveAll(runtimeRoot)
		var cache *runnerCacheStore
		if len(job.SecretValues) == 0 && job.RepositoryID != nil && *job.RepositoryID != "" {
			cacheStep := false
			for _, step := range job.Steps {
				cacheStep = cacheStep || step.Cache
			}
			if cacheStep {
				root, present := os.LookupEnv("CO_MACHINE_CACHE_DIR")
				if !present {
					if path, err := configPath(); err == nil {
						root = filepath.Join(filepath.Dir(path), "runner-cache")
					}
				}
				if store, err := openRunnerCache(root, job.CheckoutURL+"\x00"+*job.RepositoryID, image); err == nil {
					cache = &store
				}
			}
		}
		outputBytes := 0
		for _, step := range job.Steps {
			if jobCtx.Err() != nil {
				break
			}
			cacheRoot := ""
			if cache != nil {
				cacheRoot = cache.namespace
			}
			result := executeRunnerStep(jobCtx, image, filepath.Join(root, "source"), step, job, logs, runtimeRoot, cacheRoot)
			if size := runnerOutputSize(result.Outputs); outputBytes+size > 65_536 {
				result.Outputs = nil
			} else {
				outputBytes += size
			}
			steps = append(steps, result)
			if result.ExitCode != 0 {
				break
			}
		}
		complete := len(steps) == len(job.Steps) && jobCtx.Err() == nil
		for _, step := range steps {
			complete = complete && step.ExitCode == 0
		}
		if complete && job.Artifact != nil {
			data, err := collectRunnerArtifact(filepath.Join(root, "source"), *job.Artifact)
			if err == nil {
				err = uploadRunnerArtifact(jobCtx, machine, job, data)
			}
			if err != nil {
				artifactFailed = true
				if len(steps) > 0 {
					index := len(steps) - 1
					logs.append(index, "\nArtifact publication failed. Check the output directory, limits and storage availability.")
					steps[index].ExitCode, steps[index].Outputs = 1, nil
					steps[index].Output = logs.finish(index, 1)
				}
			}
		}
		if complete && !artifactFailed && job.CacheWrite && cache != nil && jobCtx.Err() == nil {
			publications, _ = cache.collect(filepath.Join(root, "source"), runtimeRoot)
		}
		return nil
	}
	if err := run(); err != nil {
		name := "Checkout"
		if len(job.Steps) > 0 {
			name = job.Steps[0].Name
		}
		private := append([]string{machine.Credential, job.Token}, job.SecretValues...)
		return []runnerStepResult{{Name: name, ExitCode: 1, Output: redactAgentError(err, private...).Error()}}, false, nil
	}
	success := jobCtx.Err() == nil && !artifactFailed && len(steps) == len(job.Steps)
	for _, step := range steps {
		success = success && step.ExitCode == 0
	}
	return steps, success, publications
}

func containsRunnerImageDigest(image string) bool {
	return strings.Contains(image, "@sha256:")
}
