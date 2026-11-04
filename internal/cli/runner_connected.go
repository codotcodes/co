package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type synchronizedRunnerWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *synchronizedRunnerWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

func synchronizeRunnerWriter(writer io.Writer) io.Writer {
	if _, ok := writer.(*synchronizedRunnerWriter); ok {
		return writer
	}
	return &synchronizedRunnerWriter{writer: writer}
}

type runnerWorker struct {
	cpus, memory uint32
	done         chan struct{}
}

type runnerEngine struct {
	ctx             context.Context
	cancel          context.CancelFunc
	workers         []runnerWorker
	all             sync.WaitGroup
	cpus            uint32
	memory          uint64
	githubAvailable *bool
	timings         runnerTimings
	stderr          io.Writer
}

func newRunnerEngine(ctx context.Context, stderr io.Writer) (*runnerEngine, error) {
	cpus := machineCPUCount()
	memory, err := runnerHostMemoryBudget()
	if err != nil {
		return nil, err
	}
	if cpus <= 0 || memory == 0 || cpus > 1024 || memory > 16_777_216 {
		return nil, errors.New("machine has no supported CPU or memory budget")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &runnerEngine{ctx: ctx, cancel: cancel, cpus: uint32(cpus), memory: memory, timings: defaultRunnerTimings, stderr: synchronizeRunnerWriter(stderr)}, nil
}

func runnerMemoryBudget(memory uint64, cpus, requested uint32) uint32 {
	scaled := uint64(math.MaxUint64)
	if memory <= math.MaxUint64/4 {
		scaled = memory * 4
	}
	scaled /= 5
	hi, lo := bits.Mul64(scaled, uint64(requested))
	denominator := uint64(max(1, cpus))
	result := uint64(math.MaxUint64)
	if hi < denominator {
		result, _ = bits.Div64(hi, lo, denominator)
	}
	return uint32(min(uint64(math.MaxUint32), max(uint64(1), result)))
}

func (runner *runnerEngine) close() {
	runner.cancel()
	runner.all.Wait()
}

func (runner *runnerEngine) capabilities() []string {
	capabilities := []string{"workflow-environment-v1", "cpu-slots-v1", "repository-actions-v1"}
	if runtime.GOOS != "windows" {
		capabilities = append(capabilities, "workflow-artifacts-v1", "repository-action-outputs-v1")
		if runner.githubAvailable == nil {
			image := runnerDefaultImage
			if value, present := os.LookupEnv("CO_MACHINE_IMAGE"); present {
				image = value
			}
			available := false
			if containsRunnerImageDigest(image) {
				ctx, cancel := context.WithTimeout(runner.ctx, 10*time.Second)
				data, err := exec.CommandContext(ctx, "podman", "image", "inspect", "--format", "{{ index .Config.Labels \"io.co.codes.github-actions\" }}", image).Output()
				cancel()
				available = err == nil && strings.TrimSpace(string(data)) == "1"
			}
			runner.githubAvailable = &available
		}
		if *runner.githubAvailable {
			capabilities = append(capabilities, "github-actions-v1", "workflow-cache-v1")
		}
	}
	if rootlessRunnerOutbound(runner.ctx) {
		capabilities = append(capabilities, "repository-actions-outbound-v1")
	}
	return capabilities
}

func (runner *runnerEngine) poll(machine machineIdentity) error {
	if runner.ctx.Err() != nil {
		return runner.ctx.Err()
	}
	active := make([]runnerWorker, 0, len(runner.workers))
	used, memoryUsed := uint32(0), uint64(0)
	for _, worker := range runner.workers {
		select {
		case <-worker.done:
		default:
			active = append(active, worker)
			used += worker.cpus
			memoryUsed += uint64(worker.memory)
		}
	}
	runner.workers = active
	if used >= runner.cpus {
		return nil
	}
	requestedAt := time.Now()
	response, err := requestAPI(runner.ctx, machine.API, "POST", "/machine-enrollment/next-job", runnerMachineCredential(machine), map[string]any{"capabilities": runner.capabilities()})
	if err != nil {
		return err
	}
	if response.StatusCode == 204 {
		response.Body.Close()
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return redactAgentError(err, machine.Credential)
	}
	record, err := apiRecord(response, []string{"job"}, "invalid API response")
	if err != nil {
		return err
	}
	job, err := decodeRunnerAssignment(record["job"])
	if err != nil {
		return err
	}
	if privateJSON(job.ID, []string{machine.Credential}) {
		return errors.New("invalid job assignment")
	}
	for _, step := range job.Steps {
		if privateJSON(step.Name, []string{machine.Credential}) {
			return finishRunnerJob(runner.ctx, machine, job, nil, false)
		}
	}
	if !validRunnerAssignment(job, machine.API) || job.VCPUs > runner.cpus-used {
		return finishRunnerJob(runner.ctx, machine, job, nil, false)
	}
	job.RAMMiB = runnerMemoryBudget(runner.memory, runner.cpus, job.VCPUs)
	maximumMemory := max(uint64(1), runner.memory*4/5)
	if uint64(job.RAMMiB)+memoryUsed > maximumMemory {
		return finishRunnerJob(runner.ctx, machine, job, nil, false)
	}
	job.MachineID = machine.ID
	done := make(chan struct{})
	runner.workers = append(runner.workers, runnerWorker{cpus: job.VCPUs, memory: job.RAMMiB, done: done})
	runner.all.Add(1)
	go func() {
		defer runner.all.Done()
		defer close(done)
		fmt.Fprintf(runner.stderr, "Running job %s on machine %s\n", job.ID, machine.ID)
		logs := &runnerLogs{}
		logCtx, stopLogs := context.WithCancel(runner.ctx)
		logDone := make(chan struct{})
		go func() {
			defer close(logDone)
			uploadRunnerLogs(logCtx, machine, job.ID, job.Token, logs, runner.stderr)
		}()
		steps, success, publications := executeRunnerJob(runner.ctx, machine, job, requestedAt, runner.timings, logs)
		stopLogs()
		<-logDone
		if err := finishRunnerJob(runner.ctx, machine, job, steps, success); err != nil {
			fmt.Fprintf(runner.stderr, "Job %s: %s\n", job.ID, err)
		} else if success && runner.ctx.Err() == nil {
			for _, publication := range publications {
				if err := publication.publish(); err != nil {
					fmt.Fprintf(runner.stderr, "Job %s: cache save skipped\n", job.ID)
				}
			}
		}
	}()
	return nil
}

func prepareConnectedRunner(ctx context.Context, id string, stderr io.Writer) (*os.File, error) {
	if !validAgentIdentity(agentIdentity{ID: id, Name: "machine"}) {
		return nil, errors.New("invalid machine ID")
	}
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	file, err := openRunnerEnrollmentLock(filepath.Join(filepath.Dir(path), "runner-"+id+".lock"))
	if err != nil {
		return nil, err
	}
	locked, err := tryConfigLock(file)
	if err != nil || !locked {
		file.Close()
		return nil, errors.New("machine runner is already connected")
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, "podman", "rm", "--force", "--time=0", "--filter", "label=io.co.codes.machine="+id)
	command.Stdout, command.Stderr = stderr, stderr
	if err := command.Run(); err != nil {
		file.Close()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, errors.New("unable to stop previous job containers")
		}
		return nil, fmt.Errorf("unable to clean up previous jobs: %s", processError(err))
	}
	return file, nil
}

func runConnectedMachine(ctx context.Context, id string, stderr io.Writer) error {
	stderr = synchronizeRunnerWriter(stderr)
	fmt.Fprintf(stderr, "Machine %s connected. Stop with Ctrl-C.\n", id)
	var runner *runnerEngine
	var lock *os.File
	defer func() {
		if runner != nil {
			runner.close()
		}
		if lock != nil {
			_ = unlockConfig(lock)
			_ = lock.Close()
		}
	}()
	nextHeartbeat := time.Time{}
	for ctx.Err() == nil {
		if !time.Now().Before(nextHeartbeat) {
			temporary, err := heartbeatMachine(ctx, id)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if !temporary {
					return err
				}
				fmt.Fprintf(stderr, "%s; retrying in 30 seconds\n", err)
			} else if runner == nil {
				lock, err = prepareConnectedRunner(ctx, id, stderr)
				if err != nil {
					return err
				}
				runner, err = newRunnerEngine(ctx, stderr)
				if err != nil {
					return err
				}
			}
			nextHeartbeat = time.Now().Add(30 * time.Second)
		}
		if runner != nil {
			config, err := loadConfig()
			if err != nil {
				return err
			}
			machine, err := selectMachine(config, id)
			if err != nil {
				return err
			}
			if err := runner.poll(machine); err != nil && ctx.Err() == nil {
				fmt.Fprintln(stderr, err)
			}
		}
		if err := waitForDevice(ctx, time.Second); err != nil {
			return err
		}
	}
	return ctx.Err()
}
