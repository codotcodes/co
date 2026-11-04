package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const runnerLogStepBytes = 16 * 1024
const runnerLogJobBytes = 128 * 1024
const runnerLogTruncated = "\n[Output truncated]\n"

type runnerLiveStep struct {
	Name      string `json:"name"`
	ExitCode  *int   `json:"exitCode"`
	Output    string `json:"output"`
	truncated bool
}

type runnerLogs struct {
	mu    sync.Mutex
	steps []runnerLiveStep
}

func (logs *runnerLogs) begin(name string) int {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	index := len(logs.steps)
	logs.steps = append(logs.steps, runnerLiveStep{Name: name})
	return index
}

func normalizedRunnerLog(text string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return -1
		}
		return character
	}, text)
}

func (logs *runnerLogs) append(index int, text string) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	used := 0
	for _, step := range logs.steps {
		used += len(step.Output)
	}
	remaining := max(0, runnerLogJobBytes-used-max(0, 20-len(logs.steps))*len(runnerLogTruncated))
	step := &logs.steps[index]
	if step.truncated {
		return
	}
	budget := min(runnerLogStepBytes-len(step.Output), remaining)
	text = normalizedRunnerLog(text)
	available := max(0, budget-len(runnerLogTruncated))
	end := min(len(text), available)
	for end > 0 && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	step.Output += text[:end]
	if end < len(text) {
		step.Output += runnerLogTruncated
		step.truncated = true
	}
}

func (logs *runnerLogs) finish(index, code int) string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	logs.steps[index].ExitCode = &code
	return logs.steps[index].Output
}

func (logs *runnerLogs) snapshot() []runnerLiveStep {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return append([]runnerLiveStep{}, logs.steps...)
}

func canonicalRunnerJSON(value any) []byte {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		panic("runner JSON contains an unsupported internal type")
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	var result bytes.Buffer
	for index := 0; index < len(data); index++ {
		if data[index] == '\\' && index+1 < len(data) {
			if index+6 <= len(data) && (string(data[index:index+6]) == `\u2028` || string(data[index:index+6]) == `\u2029`) {
				if data[index+5] == '8' {
					result.WriteRune('\u2028')
				} else {
					result.WriteRune('\u2029')
				}
				index += 5
			} else {
				result.Write(data[index : index+2])
				index++
			}
		} else {
			result.WriteByte(data[index])
		}
	}
	return result.Bytes()
}

func runnerLogHash(steps []runnerLiveStep) string {
	tuples := make([][3]any, 0, len(steps))
	for _, step := range steps {
		tuples = append(tuples, [3]any{step.Name, step.ExitCode, step.Output})
	}
	return runnerDigest(canonicalRunnerJSON(tuples))
}

func runnerLogDelta(previous, next []runnerLiveStep, repair *string) map[string]any {
	steps := make([]any, 0, len(next))
	for index, step := range next {
		offset := 0
		if repair == nil && index < len(previous) {
			old := previous[index]
			sameCode := old.ExitCode == nil && step.ExitCode == nil || old.ExitCode != nil && step.ExitCode != nil && *old.ExitCode == *step.ExitCode
			if old.Name == step.Name && old.Output == step.Output && old.truncated == step.truncated && sameCode {
				continue
			}
			offset = len(old.Output)
		}
		steps = append(steps, map[string]any{"index": index, "offset": offset, "name": step.Name, "exitCode": step.ExitCode, "output": step.Output[offset:]})
	}
	base := runnerLogHash(previous)
	if repair != nil {
		base = *repair
	}
	return map[string]any{"baseHash": base, "hash": runnerLogHash(next), "reset": repair != nil, "steps": steps}
}

type runnerSecretMasker struct {
	patterns []string
	pending  string
}

func newRunnerSecretMasker(secrets []string) *runnerSecretMasker {
	masker := &runnerSecretMasker{}
	seen := map[string]bool{}
	for _, secret := range secrets {
		for _, part := range append([]string{secret}, strings.Split(secret, "\n")...) {
			normalized := normalizedRunnerLog(part)
			if normalized != "" && !seen[normalized] {
				masker.patterns = append(masker.patterns, normalized)
				seen[normalized] = true
			}
		}
	}
	return masker
}

func (masker *runnerSecretMasker) feed(text string, finish bool) string {
	masker.pending += normalizedRunnerLog(text)
	retain := 0
	if !finish {
		for _, pattern := range masker.patterns {
			for length := min(len(masker.pending), len(pattern)-1); length > retain; length-- {
				start := len(masker.pending) - length
				if utf8.RuneStart(masker.pending[start]) && strings.HasPrefix(pattern, masker.pending[start:]) {
					retain = length
					break
				}
			}
		}
	}
	end := len(masker.pending) - retain
	cursor := 0
	var output strings.Builder
	for cursor < end {
		start, length := -1, 0
		for _, pattern := range masker.patterns {
			if offset := strings.Index(masker.pending[cursor:], pattern); offset >= 0 {
				position := cursor + offset
				if start < 0 || position < start || position == start && len(pattern) > length {
					start, length = position, len(pattern)
				}
			}
		}
		if start >= 0 && start < end {
			output.WriteString(masker.pending[cursor:start])
			output.WriteString("***")
			cursor = start + length
		} else {
			output.WriteString(masker.pending[cursor:end])
			cursor = end
		}
	}
	masker.pending = masker.pending[cursor:]
	return output.String()
}

func decodeRunnerLogUTF8(data []byte, finish bool) (string, []byte) {
	var text strings.Builder
	for index := 0; index < len(data); {
		first := data[index]
		if first < 128 {
			text.WriteByte(first)
			index++
			continue
		}
		length := 0
		switch {
		case first >= 0xc2 && first <= 0xdf:
			length = 2
		case first >= 0xe0 && first <= 0xef:
			length = 3
		case first >= 0xf0 && first <= 0xf4:
			length = 4
		default:
			text.WriteRune(utf8.RuneError)
			index++
			continue
		}
		consumed := 1
		valid := true
		for part := 1; part < length; part++ {
			if index+part == len(data) {
				if !finish {
					return text.String(), append([]byte(nil), data[index:]...)
				}
				valid = false
				break
			}
			character := data[index+part]
			if character < 0x80 || character > 0xbf || part == 1 && (first == 0xe0 && character < 0xa0 || first == 0xed && character >= 0xa0 || first == 0xf0 && character < 0x90 || first == 0xf4 && character >= 0x90) {
				valid = false
				break
			}
			consumed++
		}
		if valid {
			text.Write(data[index : index+length])
			index += length
		} else {
			text.WriteRune(utf8.RuneError)
			index += consumed
		}
	}
	return text.String(), nil
}

func drainRunnerLogs(stream io.Reader, logs *runnerLogs, index int, secrets []string) {
	masker := newRunnerSecretMasker(secrets)
	var pending []byte
	buffer := make([]byte, 8192)
	for {
		count, err := stream.Read(buffer)
		if count > 0 {
			pending = append(pending, buffer[:count]...)
			text, tail := decodeRunnerLogUTF8(pending, false)
			logs.append(index, masker.feed(text, false))
			pending = tail
		}
		if err != nil || count == 0 {
			break
		}
	}
	text, _ := decodeRunnerLogUTF8(pending, true)
	logs.append(index, masker.feed(text, false))
	logs.append(index, masker.feed("", true))
}

func uploadRunnerLogs(ctx context.Context, machine machineIdentity, id, token string, logs *runnerLogs, stderr io.Writer) {
	previous := []runnerLiveStep{}
	var repair *string
	warned := false
	for ctx.Err() == nil {
		snapshot := logs.snapshot()
		update := runnerLogDelta(previous, snapshot, repair)
		if len(snapshot) > 0 && (repair != nil || len(update["steps"].([]any)) > 0) {
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			response, err := requestAPI(requestCtx, machine.API, "POST", "/machine-enrollment/jobs/"+id+"/log-deltas", &apiCredential{scheme: "Bearer", secret: machine.Credential, scoped: true}, map[string]any{"assignmentToken": token, "update": update})
			if err == nil {
				status := response.StatusCode
				fields, _ := apiRecord(response, []string{"type", "hash"}, "invalid log acknowledgment")
				kind, hash := fields.stringValue("type"), fields.stringValue("hash")
				if status == 200 && kind != nil && *kind == "ack" && hash != nil && *hash == update["hash"] {
					previous, repair, warned = snapshot, nil, false
				} else if status == 409 {
					if kind == nil || *kind != "resync" {
						cancel()
						return
					}
					if hash != nil && len(*hash) == 64 {
						valid := true
						for _, character := range []byte(*hash) {
							valid = valid && (character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F')
						}
						if valid {
							repair = hash
						}
					}
				} else if status == 401 || status == 404 {
					cancel()
					return
				} else if status != 200 {
					err = fmt.Errorf("log upload status %d", status)
				}
			}
			cancel()
			if err != nil && !warned && ctx.Err() == nil {
				fmt.Fprintln(stderr, "Live log upload interrupted; retrying while the job runs.")
				warned = true
			}
		}
		if waitForDevice(ctx, 500*time.Millisecond) != nil {
			return
		}
	}
}
