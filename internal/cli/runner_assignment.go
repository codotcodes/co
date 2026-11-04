package cli

import (
	"encoding/json"
	"errors"
	"net/url"
	"runtime"
	"strings"
)

type runnerAssignment struct {
	MachineID    string            `json:"-"`
	ID           string            `json:"id"`
	Token        string            `json:"assignmentToken"`
	CheckoutURL  string            `json:"checkoutUrl"`
	CommitOID    string            `json:"commitOid"`
	Scope        string            `json:"scope"`
	Steps        []runnerStep      `json:"steps"`
	VCPUs        uint32            `json:"vcpus"`
	RAMMiB       uint32            `json:"ramMiB"`
	Environment  map[string]string `json:"environment"`
	SecretValues []string          `json:"secretValues"`
	Artifact     *runnerArtifact   `json:"artifact"`
	CacheWrite   bool              `json:"cacheWrite"`
	RepositoryID *string           `json:"repositoryId"`
}

type runnerStep struct {
	Name              string             `json:"name"`
	Run               string             `json:"run"`
	GitHub            bool               `json:"github"`
	Cache             bool               `json:"cache"`
	ID                *string            `json:"id"`
	Shell             *string            `json:"shell"`
	WorkflowNetwork   *string            `json:"workflowNetwork"`
	ActionEnvironment *map[string]string `json:"actionEnvironment"`
	ActionFiles       []runnerActionFile `json:"actionFiles"`
	ActionNetwork     *string            `json:"actionNetwork"`
	ActionOutputs     *[]string          `json:"actionOutputs"`
}

type runnerActionFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

type runnerArtifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type runnerStepResult struct {
	Name     string            `json:"name"`
	ExitCode int               `json:"exitCode"`
	Output   string            `json:"output"`
	Outputs  map[string]string `json:"outputs,omitempty"`
}

func validArtifactPath(path string) bool {
	if len(path) > 1024 {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
		for _, character := range []byte(part) {
			if !(asciiAlphanumeric(character) || strings.ContainsRune("._-", rune(character))) {
				return false
			}
		}
	}
	return true
}

func asciiAlphanumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func validRunnerArtifact(output runnerArtifact) bool {
	if len(output.Name) == 0 || len(output.Name) > 64 || output.Name[0] < 'a' || output.Name[0] > 'z' || !validArtifactPath(output.Path) {
		return false
	}
	for _, character := range []byte(output.Name) {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func validActionOutputKeys(keys []string) bool {
	if len(keys) == 0 || len(keys) > 32 {
		return false
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" || len(key) > 64 || key == "constructor" || key == "prototype" || seen[key] {
			return false
		}
		seen[key] = true
		for index, character := range []byte(key) {
			if !(character >= 'a' && character <= 'z' || index > 0 && (character >= '0' && character <= '9' || character == '_' || character == '-')) {
				return false
			}
		}
	}
	return true
}

func validRunnerEnvironment(environment map[string]string) bool {
	if len(environment) > 100 {
		return false
	}
	total := 0
	for name, value := range environment {
		total += len(value)
		if name == "" || len(name) > 100 || len(value) > 16_384 || strings.ContainsRune(value, 0) {
			return false
		}
		for index, character := range []byte(name) {
			if !(character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9') {
				return false
			}
		}
		for _, prohibited := range []string{"HOME", "PATH", "ENV", "BASH_ENV", "IFS", "CDPATH", "SHELLOPTS", "BASHOPTS", "__proto__", "constructor", "prototype"} {
			if name == prohibited {
				return false
			}
		}
		if strings.HasPrefix(name, "CO_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			return false
		}
	}
	return total <= 65_536
}

func validRunnerSecrets(job runnerAssignment) bool {
	if !validRunnerEnvironment(job.Environment) || len(job.SecretValues) > 100 {
		return false
	}
	for _, secret := range job.SecretValues {
		if secret == "" {
			return false
		}
		found := false
		for _, value := range job.Environment {
			found = found || value == secret
		}
		for _, step := range job.Steps {
			if step.ActionEnvironment != nil {
				for _, value := range *step.ActionEnvironment {
					found = found || value == secret
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func runnerStepNetwork(job runnerAssignment, step runnerStep) (string, bool) {
	allowedScope := job.Scope == "public" || job.Scope == "owner"
	if step.WorkflowNetwork != nil {
		return "slirp4netns:allow_host_loopback=false", *step.WorkflowNetwork == "outbound" && step.GitHub && allowedScope
	}
	if step.ActionNetwork == nil {
		return "none", true
	}
	return "slirp4netns:allow_host_loopback=false", *step.ActionNetwork == "outbound" && step.ActionEnvironment != nil && allowedScope
}

func validRunnerAssignment(job runnerAssignment, apiOrigin string) bool {
	checkout, err := url.Parse(job.CheckoutURL)
	if err != nil {
		return false
	}
	api, err := url.Parse(apiOrigin)
	if err != nil {
		return false
	}
	apiHost, checkoutHost := strings.ToLower(api.Hostname()), strings.ToLower(checkout.Hostname())
	localAPI := apiHost == "localhost" || apiHost == "127.0.0.1" || strings.HasSuffix(apiHost, ".localhost")
	hostedGit := api.Scheme == "https" && (apiHost == "api.co.codes" && (checkoutHost == "git.co.codes" || checkoutHost == "co.codes") || apiHost == "api.codevved.com" && checkoutHost == "git.codevved.com")
	trusted := checkout.Scheme == "https" && (checkout.Port() == "" || checkout.Port() == "443") && hostedGit || localAPI && checkout.Scheme == "http" && (checkoutHost == "127.0.0.1" || checkoutHost == "localhost")
	if !trusted || !strings.HasSuffix(checkout.EscapedPath(), ".git") || checkout.RawQuery != "" || checkout.ForceQuery || strings.Contains(job.CheckoutURL, "#") || checkout.User != nil {
		return false
	}
	if len(job.CommitOID) < 40 || len(job.CommitOID) > 64 || job.VCPUs == 0 || job.RAMMiB == 0 || !cleanCredential(job.Token) || len(job.Steps) > 20 || !validRunnerSecrets(job) {
		return false
	}
	if !validAgentIdentity(agentIdentity{ID: job.ID, Name: "job"}) {
		return false
	}
	for _, character := range []byte(job.CommitOID) {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	totalFiles := uint64(0)
	for _, step := range job.Steps {
		if step.Cache && !step.GitHub || len(step.Run) > 65_536 || strings.ContainsRune(step.Run, 0) || step.Shell != nil && *step.Shell != "sh" && *step.Shell != "bash" {
			return false
		}
		if step.ID != nil {
			if len(*step.ID) == 0 || len(*step.ID) > 64 {
				return false
			}
			for _, character := range []byte(*step.ID) {
				if !asciiAlphanumeric(character) && character != '_' && character != '-' {
					return false
				}
			}
		}
		if step.ActionOutputs != nil && (runtime.GOOS == "windows" || step.ActionEnvironment == nil || !validActionOutputKeys(*step.ActionOutputs)) {
			return false
		}
		if _, ok := runnerStepNetwork(job, step); !ok {
			return false
		}
		if step.ActionEnvironment != nil && !validRunnerEnvironment(*step.ActionEnvironment) || len(step.ActionFiles) > 4096 || len(step.ActionFiles) > 0 && step.ActionEnvironment == nil {
			return false
		}
		for _, file := range step.ActionFiles {
			if !validArtifactPath(file.Path) || len(file.SHA256) != 64 {
				return false
			}
			for _, character := range []byte(file.SHA256) {
				if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F') {
					return false
				}
			}
			totalFiles += uint64(len(file.Content))
		}
	}
	return totalFiles <= 45*1024*1024 && (job.Artifact == nil || runtime.GOOS != "windows" && validRunnerArtifact(*job.Artifact) && len(job.SecretValues) == 0)
}

func decodeRunnerAssignment(data []byte) (runnerAssignment, error) {
	invalid := errors.New("invalid job assignment")
	var job runnerAssignment
	fields := []string{"id", "assignmentToken", "checkoutUrl", "commitOid", "scope", "steps", "vcpus", "ramMiB", "environment", "secretValues", "artifact", "cacheWrite", "repositoryId"}
	record, err := apiObject(data, fields, "invalid job assignment")
	if err != nil {
		return job, invalid
	}
	for _, name := range []string{"id", "assignmentToken", "checkoutUrl", "commitOid"} {
		if err := configField(record, name, "string", "", false); err != nil {
			return job, invalid
		}
	}
	for _, name := range []string{"vcpus", "ramMiB"} {
		if err := configField(record, name, "uint", "", false); err != nil {
			return job, invalid
		}
	}
	if err := configField(record, "scope", "string", `""`, false); err != nil {
		return job, invalid
	}
	if err := configField(record, "cacheWrite", "bool", "false", false); err != nil {
		return job, invalid
	}
	if err := configField(record, "secretValues", "strings", "[]", false); err != nil {
		return job, invalid
	}
	if err := configField(record, "repositoryId", "string", "null", true); err != nil {
		return job, invalid
	}
	if _, exists := record["environment"]; !exists {
		record["environment"] = json.RawMessage(`{}`)
	}
	var environment map[string]json.RawMessage
	if err := json.Unmarshal(record["environment"], &environment); err != nil || environment == nil {
		return job, invalid
	}
	for _, value := range environment {
		if string(value) == "null" {
			return job, invalid
		}
	}
	var steps []json.RawMessage
	if err := json.Unmarshal(record["steps"], &steps); err != nil || steps == nil {
		return job, invalid
	}
	for _, raw := range steps {
		step, err := apiObject(raw, []string{"name", "run", "github", "cache", "id", "shell", "workflowNetwork", "actionEnvironment", "actionFiles", "actionNetwork", "actionOutputs"}, "invalid job assignment")
		if err != nil {
			return job, invalid
		}
		for _, name := range []string{"name", "run"} {
			if err := configField(step, name, "string", "", false); err != nil {
				return job, invalid
			}
		}
		for _, name := range []string{"github", "cache"} {
			if err := configField(step, name, "bool", "false", false); err != nil {
				return job, invalid
			}
		}
		for _, name := range []string{"id", "shell", "workflowNetwork", "actionNetwork"} {
			if err := configField(step, name, "string", "null", true); err != nil {
				return job, invalid
			}
		}
		if err := configField(step, "actionOutputs", "strings", "null", true); err != nil {
			return job, invalid
		}
		if value, exists := step["actionEnvironment"]; exists && string(value) != "null" {
			var values map[string]json.RawMessage
			if err := json.Unmarshal(value, &values); err != nil || values == nil {
				return job, invalid
			}
			for _, raw := range values {
				if string(raw) == "null" {
					return job, invalid
				}
			}
		}
		files := []json.RawMessage{}
		if value, exists := step["actionFiles"]; exists {
			if err := json.Unmarshal(value, &files); err != nil || files == nil {
				return job, invalid
			}
		}
		for _, raw := range files {
			file, err := apiObject(raw, []string{"path", "content", "sha256"}, "invalid job assignment")
			if err != nil {
				return job, invalid
			}
			for _, key := range []string{"path", "content", "sha256"} {
				if err := configField(file, key, "string", "", false); err != nil {
					return job, invalid
				}
			}
		}
	}
	if artifact, exists := record["artifact"]; exists && string(artifact) != "null" {
		object, err := apiObject(artifact, []string{"name", "path"}, "invalid job assignment")
		if err != nil || len(object) != 2 {
			return job, invalid
		}
		for _, key := range []string{"name", "path"} {
			if err := configField(object, key, "string", "", false); err != nil {
				return job, invalid
			}
		}
	}
	// Named objects are the current wire contract; positional legacy structs are
	// validated above but not accepted as runnable assignments.
	if err := json.Unmarshal(data, &job); err != nil {
		return job, invalid
	}
	return job, nil
}
