package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCompiledAgentAccessAndAttestationRustParity(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	oid, second, third := strings.Repeat("A", 40), strings.Repeat("b", 64), strings.Repeat("c", 40)
	for _, scenario := range []string{"view", "view-pull-only", "view-no-pull", "view-ambiguous", "view-bad-repo", "view-revoked", "view-api-error", "attest", "attest-batch", "attest-partial", "attest-no-agent", "attest-other-agent", "attest-pull-only", "attest-bad-oid", "attest-invalid-later", "attest-too-many", "attest-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			var mu sync.Mutex
			var calls []string
			writes := 0
			var api string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, request.Method+" "+request.URL.RequestURI()+" "+request.Header.Get("Authorization"))
				if request.URL.Path == "/grants/grant/token" {
					if strings.HasSuffix(scenario, "revoked") {
						writer.WriteHeader(403)
						io.WriteString(writer, `{"error":"grant revoked"}`)
						return
					}
					io.WriteString(writer, `{"accessToken":"`+fixtureAgentToken+`"}`)
					return
				}
				if request.Method == "GET" {
					if scenario == "view-api-error" {
						writer.WriteHeader(403)
						io.WriteString(writer, `{"message":"repository denied"}`)
						return
					}
					io.WriteString(writer, `{"name":"repo","description":"<bytes> 日本語","count":18446744073709551615,"urls":{"self":"`+api+`/t:`+fixtureAgentToken+`/owner/repo","clone":"https://git.co.codes/owner/repo.git"}}`)
					return
				}
				writes++
				if scenario == "attest-partial" && writes == 2 {
					writer.WriteHeader(409)
					io.WriteString(writer, `{"error":"contribution_index_pending"}`)
					return
				}
				io.WriteString(writer, `{}`)
			}))
			defer server.Close()
			api = server.URL
			config := fullConfig(api)
			args := []string{"access", "view", "owner/repo"}
			agent := ""
			if strings.HasPrefix(scenario, "attest") {
				args = []string{"agent", "attest", "owner/repo", oid}
				agent = "agent"
			}
			switch scenario {
			case "view-pull-only", "attest-pull-only":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant { grants[0].Operations = []string{"pull"}; return grants })
			case "view-no-pull":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant { grants[0].Operations = []string{"push"}; return grants })
			case "view-ambiguous":
				changeGrants(t, config, func(grants []agentGrant) []agentGrant {
					other := grants[0]
					other.AgentID = "other"
					return append(grants, other)
				})
			case "view-bad-repo":
				args[2] = "owner/../repo"
			case "attest-batch":
				args = append(args, second)
			case "attest-partial":
				args = append(args, second, third)
			case "attest-no-agent":
				agent = ""
			case "attest-other-agent":
				agent = "other"
			case "attest-bad-oid":
				args[3] = "abc"
			case "attest-invalid-later":
				args = append(args, "invalid")
			case "attest-too-many":
				for range 100 {
					args = append(args, oid)
				}
			}
			writeConfigFixture(t, path, config)
			before, _ := os.ReadFile(path)
			env := []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + api}
			if agent != "" {
				env = append(env, "CO_AGENT_ID="+agent)
			}
			var expected observation
			var expectedCalls []string
			for index, binary := range []string{rust, goBinary} {
				mu.Lock()
				calls = nil
				writes = 0
				mu.Unlock()
				result := invoke(t, binary, root, args, env)
				mu.Lock()
				requests := append([]string(nil), calls...)
				mu.Unlock()
				if index == 0 {
					expected, expectedCalls = result, requests
				} else if !reflect.DeepEqual(result, expected) || !reflect.DeepEqual(requests, expectedCalls) {
					t.Fatalf("agent API differs: Go=%+v Rust=%+v", result, expected)
				}
				for _, secret := range []string{fixtureAgentToken, "fixture-lineage", "fixture-human-session"} {
					if strings.Contains(result.Stdout+result.Stderr, secret) {
						t.Fatal("agent API command exposed private credentials")
					}
				}
				for position, request := range requests {
					if position == 0 {
						if request != "POST /grants/grant/token Lineage fixture-lineage" {
							t.Fatal("wrong mint authority")
						}
					} else if strings.HasPrefix(scenario, "view") {
						if request != "GET /t:"+fixtureAgentToken+"/owner/repo " {
							t.Fatal("wrong access URL or human authorization")
						}
					} else if !strings.HasSuffix(request, " Bearer "+fixtureAgentToken) {
						t.Fatal("attestation did not use minted token")
					}
				}
				if scenario == "attest-partial" && (len(requests) != 3 || strings.Count(result.Stdout, "Attested ") != 1) {
					t.Fatal("partial attestation was replayed or continued after failure")
				}
				if scenario == "attest-invalid-later" && len(requests) != 0 {
					t.Fatal("batch mutation began before all OIDs were validated")
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) {
					t.Fatal("agent API mutated profile credentials")
				}
			}
		})
	}
	t.Log("17 compiled agent access/attestation cases match Rust, including partial batches and exact scoped request identities")
}

func TestAgentViewHonorsSelectedIdentityAndRejectsSecretDocuments(t *testing.T) {
	goBinary := goBinaryForTests(t)
	root := t.TempDir()
	path := filepath.Join(root, "profile", "config.json")
	var mode atomic.Value
	mode.Store("selected")
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method == "POST" {
			io.WriteString(writer, `{"accessToken":"`+fixtureAgentToken+`"}`)
			return
		}
		if mode.Load() == "selected" {
			io.WriteString(writer, `{"name":"repo"}`)
		} else {
			json.NewEncoder(writer).Encode(map[string]any{"unexpected": fixtureAgentToken})
		}
	}))
	defer server.Close()
	config := fullConfig(server.URL)
	changeGrants(t, config, func(grants []agentGrant) []agentGrant {
		other := grants[0]
		other.AgentID = "other"
		return append(grants, other)
	})
	writeConfigFixture(t, path, config)
	env := []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL, "CO_AGENT_ID=agent"}
	result := invoke(t, goBinary, root, []string{"access", "view", "owner/repo"}, env)
	if result.Code != 0 || calls.Load() != 2 {
		t.Fatal("selected identity did not scope the pull grant", result)
	}
	mode.Store("private")
	result = invoke(t, goBinary, root, []string{"access", "view", "owner/repo"}, env)
	if result.Code != 1 || result.Stdout != "" || strings.Contains(result.Stderr, fixtureAgentToken) {
		t.Fatal("secret-bearing document was rendered")
	}
	before := calls.Load()
	result = invoke(t, goBinary, root, []string{"access", "view", "owner/repo"}, append(env, "CO_AGENT_ID=missing"))
	if result.Code != 1 || calls.Load() != before {
		t.Fatal("selected identity silently substituted another grant")
	}
}
