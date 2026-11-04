package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentGrantSelectionExpiryAndOriginMapping(t *testing.T) {
	config := fullConfig("https://api.co.codes")
	agent := "agent"
	if _, err := selectAgentGrant(config, "owner", "repo", &agent, "push", 4000000000); err == nil {
		t.Fatal("grant allowed at exact expiry")
	}
	if _, err := selectAgentGrant(config, "owner", "repo", &agent, "push", 3999999999); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ api, want string }{{"https://api.codevved.com", "git.codevved.com"}, {"https://api.codevved.com:443///", "git.codevved.com"}, {"https://api.co.codes", "git.co.codes"}, {"http://api.codevved.com", "git.co.codes"}, {"https://API.codevved.com", "git.co.codes"}, {"https://api.codevved.com/extra", "git.co.codes"}} {
		if got := gitHostForAPI(test.api); got != test.want {
			t.Fatal("Git/API pair broadened", test.api, got)
		}
	}
}

func TestAgentMintIsFreshAndRevocationNeverUsesHuman(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, binary := range []string{rust, goBinary} {
		root := t.TempDir()
		path := filepath.Join(root, "profile", "config.json")
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Authorization") != "Lineage fixture-lineage" {
				t.Error("minting substituted human credentials")
			}
			if calls.Add(1) == 1 {
				io.WriteString(writer, `{"accessToken":"`+fixtureAgentToken+`"}`)
			} else {
				writer.WriteHeader(403)
				io.WriteString(writer, `{"error":"grant revoked"}`)
			}
		}))
		writeConfigFixture(t, path, fullConfig(server.URL))
		before, _ := os.ReadFile(path)
		env := []string{"CO_AGENT_ID=agent", "CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL}
		first := invokeInput(t, binary, root, []string{"git-credential", "get"}, env, []byte(agentGitInput))
		second := invokeInput(t, binary, root, []string{"git-credential", "get"}, env, []byte(agentGitInput))
		server.Close()
		if first.Code != 0 || !strings.Contains(first.Stdout, fixtureAgentToken) || second.Code != 1 || second.Stdout != "quit=true\n\n" || calls.Load() != 2 {
			t.Fatal("minted token was cached or revocation fell back")
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("minting persisted or altered credentials")
		}
	}
}

func TestUnsafeMintResponsesAndRedirectsNeverEmitCredentials(t *testing.T) {
	_, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	path := filepath.Join(root, "profile", "config.json")
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetCalls.Add(1)
		io.WriteString(writer, `{"accessToken":"foreign-token"}`)
	}))
	defer target.Close()
	for _, scenario := range []string{"empty", "newline", "missing", "wrong-type", "duplicate", "reflected-error", "redirect"} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch scenario {
			case "empty":
				io.WriteString(writer, `{"accessToken":""}`)
			case "newline":
				io.WriteString(writer, `{"accessToken":"token\npassword=human-fallback"}`)
			case "missing":
				io.WriteString(writer, `{}`)
			case "wrong-type":
				io.WriteString(writer, `{"accessToken":7}`)
			case "duplicate":
				io.WriteString(writer, `{"accessToken":"a","accessToken":"b"}`)
			case "reflected-error":
				writer.WriteHeader(403)
				io.WriteString(writer, `{"error_description":"fixture-lineage was refused"}`)
			case "redirect":
				http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
			}
		}))
		writeConfigFixture(t, path, fullConfig(server.URL))
		result := invokeInput(t, goBinary, root, []string{"git-credential", "get"}, []string{"CO_AGENT_ID=agent", "CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL}, []byte(agentGitInput))
		server.Close()
		if result.Code != 1 || result.Stdout != "quit=true\n\n" || strings.Contains(result.Stderr, "fixture-lineage") || strings.Contains(result.Stderr, "human-fallback") {
			t.Fatal("mint response failed open or exposed secrets", scenario)
		}
	}
	if targetCalls.Load() != 0 {
		t.Fatal("scoped mint followed a redirect")
	}
}

func TestMintCancellationAndEarlyHostRejection(t *testing.T) {
	path := isolatedConfig(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { close(started); <-request.Context().Done() }))
	defer server.Close()
	config := fullConfig(server.URL)
	t.Setenv("CO_API_URL", server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := mintAgentToken(ctx, config, "owner", "repo", nil, "push"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("mint did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled mint succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("mint ignored cancellation")
	}
	writeFixture(t, path, "invalid configuration", 0600)
	t.Setenv("CO_AGENT_ID", "agent")
	t.Setenv("CO_API_URL", "https://api.codevved.com")
	var output strings.Builder
	err := gitCredentialCommand([]string{"get"}, strings.NewReader(agentGitInput), &output)
	if err == nil || err.Error() != "agent credential request is outside git.codevved.com" || output.String() != "quit=true\n\n" {
		t.Fatal("foreign host read private config before rejection", err)
	}
}
