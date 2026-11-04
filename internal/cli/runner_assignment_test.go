package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRunnerAssignmentCompiledRustValidation(t *testing.T) {
	binary := os.Getenv("CO_TEST_RUST_RUNNER_BINARY")
	if binary == "" {
		t.Skip("set the frozen Rust runner validation probe")
	}
	type fixture struct {
		name string
		job  map[string]any
		api  string
	}
	base := map[string]any{"id": "job", "assignmentToken": "assignment", "checkoutUrl": "https://git.co.codes/owner/repo.git", "commitOid": strings.Repeat("a", 40), "steps": []any{map[string]any{"name": "build", "run": "true"}}, "vcpus": 1, "ramMiB": 256}
	var fixtures []fixture
	add := func(name string, mutate func(map[string]any), api string) {
		raw, _ := json.Marshal(base)
		var job map[string]any
		_ = json.Unmarshal(raw, &job)
		if mutate != nil {
			mutate(job)
		}
		fixtures = append(fixtures, fixture{name, job, api})
	}
	api := "https://api.co.codes"
	add("minimal", nil, api)
	for _, field := range []string{"id", "assignmentToken", "checkoutUrl", "commitOid", "steps", "vcpus", "ramMiB"} {
		add("missing-"+field, func(job map[string]any) { delete(job, field) }, api)
		add("null-"+field, func(job map[string]any) { job[field] = nil }, api)
	}
	for _, origin := range []string{"https://evil.invalid/repo.git", "https://git.co.codes:444/repo.git", "https://git.co.codes:443/repo.git", "https://co.codes/repo.git", "http://git.co.codes/repo.git", "https://git.co.codes/repo", "https://git.co.codes/repo.git?", "https://git.co.codes/repo.git#", "https://user@getter.invalid/repo.git", "file:///repo.git", "https://git.codevved.com/repo.git"} {
		add("checkout-"+origin, func(job map[string]any) { job["checkoutUrl"] = origin }, api)
	}
	add("staging", func(job map[string]any) { job["checkoutUrl"] = "https://git.codevved.com/repo.git" }, "https://api.codevved.com")
	add("localhost", func(job map[string]any) { job["checkoutUrl"] = "http://127.0.0.1:1234/repo.git" }, "http://api.co.localhost")
	add("localhost-foreign", func(job map[string]any) { job["checkoutUrl"] = "http://other.localhost/repo.git" }, "http://api.co.localhost")
	for _, oid := range []string{strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		add("oid-"+oid, func(job map[string]any) { job["commitOid"] = oid }, api)
	}
	for _, env := range []map[string]any{{"TOKEN": "secret"}, {"HOME": "bad"}, {"CO_TOKEN": "bad"}, {"LD_PRELOAD": "bad"}, {"1BAD": "bad"}, {"TOKEN": nil}, {"TOKEN": strings.Repeat("x", 16385)}} {
		add("environment", func(job map[string]any) { job["environment"] = env }, api)
	}
	add("secret-present", func(job map[string]any) {
		job["environment"] = map[string]any{"TOKEN": "secret"}
		job["secretValues"] = []string{"secret"}
	}, api)
	add("secret-absent", func(job map[string]any) { job["secretValues"] = []string{"secret"} }, api)
	add("secret-null", func(job map[string]any) { job["secretValues"] = nil }, api)
	add("empty-steps", func(job map[string]any) { job["steps"] = []any{} }, api)
	for _, fields := range []map[string]any{
		{"name": "step"}, {"run": "true"}, {"name": "step", "run": "true"},
		{"name": "step", "run": "true", "cache": true},
		{"name": "step", "run": "true", "github": true, "cache": true},
		{"name": "step", "run": "true", "shell": "fish"},
		{"name": "step", "run": "true", "shell": "bash"},
		{"name": "step", "run": "true", "actionFiles": nil},
		{"name": "step", "run": "true", "actionFiles": []any{map[string]any{"path": "src/a", "content": "eA==", "sha256": strings.Repeat("0", 64)}}},
		{"name": "step", "run": "true", "actionEnvironment": map[string]any{}, "actionFiles": []any{map[string]any{"path": "src/a", "content": "eA==", "sha256": strings.Repeat("0", 64)}}},
		{"name": "step", "run": "true", "actionEnvironment": map[string]any{}, "actionOutputs": []string{"url"}},
		{"name": "step", "run": "true", "actionEnvironment": map[string]any{}, "actionOutputs": []string{"constructor"}},
		{"name": "step", "run": "true", "actionEnvironment": map[string]any{}, "actionOutputs": []string{"url", "url"}},
		{"name": "step", "run": "true", "actionEnvironment": map[string]any{}, "actionOutputs": []string{}},
		{"name": "step", "run": "true", "actionEnvironment": map[string]any{}, "actionNetwork": "outbound"},
		{"name": "step", "run": "true", "github": true, "workflowNetwork": "outbound"},
		{"name": "step", "run": "true", "github": true, "workflowNetwork": "none"},
	} {
		add("step-fields", func(job map[string]any) { job["steps"] = []any{fields}; job["scope"] = "public" }, api)
	}
	for _, artifact := range []map[string]any{{"name": "site", "path": "dist"}, {"name": "Site", "path": "dist"}, {"name": "site", "path": "../dist"}, {"name": "site", "path": ".GIT/config"}, {"name": "site", "path": "dist", "unknown": true}} {
		add("artifact", func(job map[string]any) { job["artifact"] = artifact }, api)
	}
	var input bytes.Buffer
	for _, fixture := range fixtures {
		if err := json.NewEncoder(&input).Encode(map[string]any{"job": fixture.job, "api": fixture.api}); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(binary, "__migration-validate")
	command.Stdin = &input
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for _, fixture := range fixtures {
		if !scanner.Scan() {
			t.Fatal("oracle omitted validation result")
		}
		raw, _ := json.Marshal(fixture.job)
		job, err := decodeRunnerAssignment(raw)
		actual := err == nil && validRunnerAssignment(job, fixture.api)
		if expected := scanner.Text() == "true"; actual != expected {
			t.Errorf("assignment differs in %s: Go=%v Rust=%v error=%v", fixture.name, actual, expected, err)
		}
	}
	if scanner.Scan() || scanner.Err() != nil {
		t.Fatal("unexpected oracle output")
	}
	t.Logf("%d assignment decode/validation cases match compiled retained Rust", len(fixtures))
}

func TestRunnerAssignmentRejectsCredentialAndPathAmbiguity(t *testing.T) {
	job := runnerAssignment{ID: "job", Token: "assignment", CheckoutURL: "https://git.co.codes/owner/repo.git", CommitOID: strings.Repeat("a", 40), VCPUs: 1, RAMMiB: 256}
	if !validRunnerAssignment(job, "https://api.co.codes") {
		t.Fatal("valid minimal assignment rejected")
	}
	for _, id := range []string{"../job", "job?x", "", "job\n"} {
		copy := job
		copy.ID = id
		if validRunnerAssignment(copy, "https://api.co.codes") {
			t.Fatal("unsafe job identity accepted")
		}
	}
	job.Token = "assignment\nother"
	if validRunnerAssignment(job, "https://api.co.codes") {
		t.Fatal("unsafe assignment credential accepted")
	}
	job.Token = "assignment"
	job.Environment = map[string]string{"TOKEN": "private-value"}
	job.SecretValues = []string{"private-value"}
	job.Steps = []runnerStep{{Name: "private-value", Run: "true"}}
	if validRunnerAssignment(job, "https://api.co.codes") {
		t.Fatal("step metadata could expose a private environment value")
	}
}
