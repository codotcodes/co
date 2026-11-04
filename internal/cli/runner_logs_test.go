package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func runnerLogOracle(t *testing.T, input, actual any) {
	t.Helper()
	binary := os.Getenv("CO_TEST_RUST_RUNNER_LOGS_BINARY")
	if binary == "" {
		return
	}
	raw, _ := json.Marshal(input)
	command := exec.CommandContext(t.Context(), binary, "__migration-validate")
	command.Stdin = bytes.NewReader(append(raw, '\n'))
	data, err := command.Output()
	if err != nil {
		t.Fatal("runner log probe failed", err)
	}
	var expected, normalized any
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal("runner log probe response invalid", err)
	}
	raw, _ = json.Marshal(actual)
	_ = json.Unmarshal(raw, &normalized)
	if !reflect.DeepEqual(normalized, expected) {
		t.Fatal("runner logs differ from retained Rust")
	}
}

func TestRunnerSecretMaskerContract(t *testing.T) {
	for _, test := range []struct {
		input   string
		secrets []string
		want    string
	}{
		{"before secret after!", []string{"secret"}, "before *** after!"},
		{"sec\x00ret first\nsecond first second", []string{"secret", "first\nsecond"}, "*** *** *** ***"},
		{"🦀token and 🦀", []string{"🦀token", "🦀"}, "*** and ***"},
		{"abcabcdabcde", []string{"abc", "abcd", "bcde"}, "*********e"},
		{"normal short lines\n", nil, "normal short lines\n"},
	} {
		for split := 0; split <= len(test.input); split++ {
			if split < len(test.input) && test.input[split]&0xc0 == 0x80 {
				continue
			}
			chunks := []string{test.input[:split], test.input[split:]}
			masker := newRunnerSecretMasker(test.secrets)
			actual := []string{masker.feed(chunks[0], false), masker.feed(chunks[1], false), masker.feed("", true)}
			if strings.Join(actual, "") != test.want {
				t.Fatal("masking changed at split", split, actual)
			}
			secrets := test.secrets
			if secrets == nil {
				secrets = []string{}
			}
			runnerLogOracle(t, map[string]any{"mode": "mask", "chunks": chunks, "secrets": secrets}, actual)
		}
	}
}

func TestRunnerLogHashesDeltasAndBoundsContract(t *testing.T) {
	logs := &runnerLogs{}
	previous := []runnerLiveStep{}
	var actions, actual []any
	apply := func(action map[string]any) {
		actions = append(actions, action)
		if name, ok := action["begin"].(string); ok {
			logs.begin(name)
		}
		if index, ok := action["append"].(int); ok {
			logs.append(index, action["text"].(string))
		}
		if index, ok := action["finish"].(int); ok {
			logs.finish(index, action["code"].(int))
		}
		snapshot := logs.snapshot()
		var repair *string
		if value, ok := action["repair"].(string); ok {
			repair = &value
		}
		actual = append(actual, map[string]any{"steps": snapshot, "update": runnerLogDelta(previous, snapshot, repair)})
		previous = snapshot
	}
	apply(map[string]any{"begin": "Test"})
	apply(map[string]any{"append": 0, "text": "🦀\n"})
	if got := runnerLogHash(logs.snapshot()); got != "03032299553b8e5ce964b2b4ecbe8c3cca49ecab3919e391452793a7832c7337" {
		t.Fatal("canonical Rust tuple hash differs", got)
	}
	apply(map[string]any{"append": 0, "text": "next\n"})
	apply(map[string]any{"finish": 0, "code": 0})
	apply(map[string]any{"repair": runnerLogHash(nil)})
	for index := 1; index < 20; index++ {
		apply(map[string]any{"begin": "name\u2028\u2029 literal \\u2028"})
		apply(map[string]any{"append": index, "text": strings.Repeat("é", runnerLogStepBytes)})
	}
	total := 0
	for _, step := range logs.snapshot() {
		if len(step.Output) > runnerLogStepBytes {
			t.Fatal("step output exceeded budget")
		}
		total += len(step.Output)
	}
	if total > runnerLogJobBytes {
		t.Fatal("job output exceeded budget")
	}
	runnerLogOracle(t, map[string]any{"mode": "logs", "actions": actions, "secrets": []string{}}, actual)
}

type runnerChunkReader struct {
	chunks [][]byte
	index  int
	offset int
}

func (reader *runnerChunkReader) Read(out []byte) (int, error) {
	for reader.index < len(reader.chunks) && reader.offset == len(reader.chunks[reader.index]) {
		reader.index++
		reader.offset = 0
	}
	if reader.index == len(reader.chunks) {
		return 0, io.EOF
	}
	n := copy(out, reader.chunks[reader.index][reader.offset:])
	reader.offset += n
	return n, nil
}

func TestRunnerLogDrainingContract(t *testing.T) {
	for _, test := range []struct {
		input []byte
		want  string
	}{
		{[]byte("hello 🦀 secret\n"), "hello 🦀 ***\n"},
		{[]byte{255, 255, 0xe2, 0x82, 'x', 0xed, 0xa0, 0x80}, "���x���"},
		{[]byte{0xf0, 0x9f, 0xa6}, "�"},
		{[]byte("control\x1b\x00 sec\x00ret <text>\n"), "control *** <text>\n"},
	} {
		input := test.input
		for chunkSize := 1; chunkSize <= len(input); chunkSize++ {
			var chunks [][]byte
			var encoded []string
			for offset := 0; offset < len(input); offset += chunkSize {
				chunk := input[offset:min(offset+chunkSize, len(input))]
				chunks = append(chunks, chunk)
				encoded = append(encoded, base64.StdEncoding.EncodeToString(chunk))
			}
			logs := &runnerLogs{}
			logs.begin("stream")
			drainRunnerLogs(&runnerChunkReader{chunks: chunks}, logs, 0, []string{"secret"})
			snapshot := logs.snapshot()
			if len(snapshot) != 1 || snapshot[0].Output != test.want {
				t.Fatal("log draining changed at chunk size", chunkSize, snapshot)
			}
			runnerLogOracle(t, map[string]any{"mode": "drain", "chunks": encoded, "secrets": []string{"secret"}}, map[string]any{"steps": snapshot, "hash": runnerLogHash(snapshot)})
		}
	}
}

func TestRunnerLogUploadAckResyncAndPrivateAuthority(t *testing.T) {
	logs := &runnerLogs{}
	logs.begin("build")
	logs.append(0, "first 🦀\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var updates []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer co_runner_fixture" || r.URL.Path != "/machine-enrollment/jobs/job/log-deltas" {
			t.Error("log uploader used wrong authority")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["assignmentToken"] != "assignment" {
			t.Error("log uploader omitted assignment fence")
		}
		update, _ := body["update"].(map[string]any)
		mu.Lock()
		updates = append(updates, update)
		count := len(updates)
		mu.Unlock()
		if count == 1 {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{"type": "resync", "hash": strings.Repeat("a", 64)})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"type": "ack", "hash": update["hash"]})
			if count == 2 {
				logs.append(0, "next\n")
			} else {
				cancel()
			}
		}
	}))
	defer server.Close()
	var stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		uploadRunnerLogs(ctx, machineIdentity{API: server.URL, Credential: "co_runner_fixture"}, "job", "assignment", logs, &stderr)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("log uploader failed to converge after resync")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 3 || updates[1]["baseHash"] != strings.Repeat("a", 64) || updates[1]["reset"] != true || updates[2]["reset"] != false {
		t.Fatal("log repair did not reset then resume deltas")
	}
	steps := updates[2]["steps"].([]any)
	if steps[0].(map[string]any)["output"] != "next\n" || stderr.Len() != 0 {
		t.Fatal("ack did not advance the append-only prefix")
	}
}
