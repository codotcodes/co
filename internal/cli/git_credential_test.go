package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const agentGitInput = "protocol=https\nhost=git.co.codes\npath=owner/repo.git\n\n"
const fixtureAgentToken = "fixture-minted-agent-token"

func changeGrants(t *testing.T, config configuration, update func([]agentGrant) []agentGrant) {
	t.Helper()
	var grants []agentGrant
	if err := json.Unmarshal(config["agent_grants"], &grants); err != nil {
		t.Fatal(err)
	}
	config["agent_grants"], _ = json.Marshal(update(grants))
}

func TestCompiledGitCredentialRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"agent", "without-suffix", "crlf", "double-crlf", "duplicate-fields", "human", "no-session", "store", "erase", "bad-operation", "foreign-host", "wrong-port", "wrong-scheme", "staging-host", "missing-path", "foreign-repo", "invalid-repo", "unknown-agent", "empty-agent", "invalid-agent", "expired", "pull-only", "ambiguous", "other-agent", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				calls = append(calls, request.Method+" "+request.URL.RequestURI()+" "+request.Header.Get("Authorization")+" "+request.UserAgent())
				mu.Unlock()
				if scenario == "revoked" {
					writer.WriteHeader(403)
					io.WriteString(writer, `{"error":"grant revoked"}`)
					return
				}
				io.WriteString(writer, `{"accessToken":"`+fixtureAgentToken+`"}`)
			}))
			defer server.Close()
			config := fullConfig(server.URL)
			input, operation, agent := agentGitInput, "get", "agent"
			human := false
			switch scenario {
			case "without-suffix":
				input = strings.ReplaceAll(input, "repo.git", "repo")
			case "crlf":
				input = strings.ReplaceAll(input, "\n", "\r\n")
			case "double-crlf":
				input = strings.ReplaceAll(input, "\n", "\r\r\n")
			case "duplicate-fields":
				input = "host=foreign.invalid\n" + input + "host=ignored.invalid\n"
			case "human":
				human = true
			case "no-session":
				human = true
				config["session_token"] = json.RawMessage("null")
			case "store", "erase":
				operation = scenario
				agent = "malformed!"
			case "bad-operation":
				operation = "invalid"
			case "foreign-host":
				input = strings.ReplaceAll(input, "git.co.codes", "foreign.invalid")
			case "wrong-port":
				input = strings.ReplaceAll(input, "git.co.codes", "git.co.codes:444")
			case "wrong-scheme":
				input = strings.ReplaceAll(input, "https", "http")
			case "staging-host":
				input = strings.ReplaceAll(input, "git.co.codes", "git.codevved.com")
			case "missing-path":
				input = "protocol=https\nhost=git.co.codes\n\n"
			case "foreign-repo":
				input = strings.ReplaceAll(input, "owner/repo", "other/repo")
			case "invalid-repo":
				input = strings.ReplaceAll(input, "owner/repo", "owner/../repo")
			case "unknown-agent":
				agent = "other"
			case "empty-agent":
				agent = ""
			case "invalid-agent":
				agent = "bad_id"
			case "expired":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant { grants[0].Expires = 0; return grants })
			case "pull-only":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant { grants[0].Operations = []string{"pull"}; return grants })
			case "ambiguous":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant { return append(grants, grants[0]) })
			case "other-agent":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant {
					other := grants[0]
					other.AgentID = "other"
					return append(grants, other)
				})
			}
			writeConfigFixture(t, path, config)
			env := []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL}
			if !human {
				env = append(env, "CO_AGENT_ID="+agent)
			}
			want := invokeInput(t, rust, root, []string{"git-credential", operation}, env, []byte(input))
			actual := invokeInput(t, goBinary, root, []string{"git-credential", operation}, env, []byte(input))
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("credential behavior differs: Go=%+v Rust=%+v", actual, want)
			}
			if !human && operation == "get" && actual.Code != 0 && actual.Stdout != "quit=true\n\n" {
				t.Fatal("agent failure did not stop helper lookup")
			}
			if strings.Contains(actual.Stderr, "fixture-human-session") || strings.Contains(actual.Stderr, "fixture-lineage") {
				t.Fatal("private credentials leaked")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != 0 {
				if len(calls) != 2 || calls[0] != calls[1] || calls[0] != "POST /grants/grant/token Lineage fixture-lineage co/"+Version {
					t.Fatal("minting request identity differs")
				}
			}
		})
	}
	t.Log("25 compiled Git credential cases match Rust protocol output and scoped minting requests")
}

func TestAgentCredentialInputAndAPIBindingFailClosed(t *testing.T) {
	_, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	path := filepath.Join(root, "profile", "config.json")
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		io.WriteString(writer, `{"accessToken":"`+fixtureAgentToken+`"}`)
	}))
	defer server.Close()
	for _, scenario := range []string{"foreign-binding", "unbound-override", "invalid-utf8", "large-input", "path-in-grant"} {
		config := fullConfig(server.URL)
		input := []byte(agentGitInput)
		switch scenario {
		case "foreign-binding":
			config["api_url"] = json.RawMessage(`"https://other.example.invalid"`)
		case "unbound-override":
			config["api_url"] = json.RawMessage("null")
		case "invalid-utf8":
			input = []byte{255}
		case "large-input":
			input = bytes.Repeat([]byte("x"), (1<<20)+1)
		case "path-in-grant":
			changeGrants(t, config, func(grants []agentGrant) []agentGrant { grants[0].GrantID = "../other"; return grants })
		}
		writeConfigFixture(t, path, config)
		result := invokeInput(t, goBinary, root, []string{"git-credential", "get"}, []string{"CO_AGENT_ID=agent", "CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL}, input)
		if result.Code != 1 || result.Stdout != "quit=true\n\n" {
			t.Fatal("agent request failed open", scenario)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatal("unsafe request transmitted credentials")
	}
}

func TestRealGitStopsBeforeHumanHelper(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, binary := range []string{rust, goBinary} {
		root := t.TempDir()
		path := filepath.Join(root, "profile", "config.json")
		var mode atomic.Value
		mode.Store("")
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if mode.Load() == "revoked" {
				writer.WriteHeader(403)
				io.WriteString(writer, `{"error":"grant revoked"}`)
				return
			}
			t.Error("out-of-scope helper attempted minting")
			writer.WriteHeader(403)
		}))
		defer server.Close()
		writeConfigFixture(t, path, fullConfig(server.URL))
		first, human, marker := filepath.Join(root, "agent-helper"), filepath.Join(root, "human-helper"), filepath.Join(root, "human-ran")
		writeFixture(t, first, "#!/bin/sh\nexec '"+binary+"' git-credential \"$@\"\n", 0500)
		writeFixture(t, human, "#!/bin/sh\nprintf marker > '"+marker+"'\nprintf 'username=human\\npassword=human-fallback\\n\\n'\n", 0500)
		for _, scope := range []string{"foreign-repo", "missing-path", "revoked"} {
			mode.Store(scope)
			request := "protocol=https\nhost=git.co.codes\npath=other/repo.git\n\n"
			usePath := "true"
			if scope == "missing-path" {
				usePath = "false"
				request = agentGitInput
			}
			if scope == "revoked" {
				request = agentGitInput
			}
			command := exec.Command("git", "-c", "credential.helper=", "-c", "credential.helper="+first, "-c", "credential.helper="+human, "-c", "credential.useHttpPath="+usePath, "credential", "fill")
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL, "CO_AGENT_ID=agent", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
			command.Stdin = strings.NewReader(request)
			output, err := command.CombinedOutput()
			if err == nil || strings.Contains(string(output), "human-fallback") {
				t.Fatal("Git used fallback credentials")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("Git invoked the human helper", err)
			}
		}
	}
	t.Log("real Git stops helper chaining on scoped agent failure for Rust and Go")
}
