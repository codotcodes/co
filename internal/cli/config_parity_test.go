package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCompiledConfigDefaultsAndValidation(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	path := filepath.Join(root, "profile", "config.json")
	sources := []string{
		`{}`,
		`[]`,
		`{"agents":[["agent","Fixture"]]}`,
		`{"upstream_name":"\ud83d\ude00"}`,
		`{"agents":[{"id":"agent","name":"Fixture"}],"pending_agent_requests":[{"id":"request","agent_id":"agent","owner":"owner","repo":"repo","poll_token":"fixture","requested_expires_unix":18446744073709551615}],"agent_grants":[{"agent_id":"agent","owner":"owner","repo":"repo","grant_id":"grant","lineage_token":"fixture","expires_unix":18446744073709551615}]}`,
	}
	for _, source := range sources {
		var wantResult observation
		var wantConfig any
		for index, binary := range []string{rust, goBinary} {
			writeFixture(t, path, source, 0600)
			result := invoke(t, binary, root, []string{"logout"}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path)})
			if result.Code != 0 {
				t.Fatal(result)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(source, "18446744073709551615") && !bytes.Contains(data, []byte("18446744073709551615")) {
				t.Fatal("uint64 credential expiry lost precision")
			}
			var config any
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if index == 0 {
				wantResult, wantConfig = result, config
			} else if !reflect.DeepEqual(result, wantResult) || !reflect.DeepEqual(config, wantConfig) {
				t.Fatal("config defaults or integer precision differ")
			}
		}
	}
	invalid := []string{`null`, `{`, `{"jj":null}`, `{"session_token":3}`, `{"agents":null}`, `{"agents":[{}]}`, `{"pending_agent_submissions":[{}]}`, `{"pending_agent_requests":[{"id":"r","agent_id":"a","owner":"o","repo":"r","requested_expires_unix":1}]}`, `{"agent_grants":[{}]}`, `{"machines":[{}]}`, `{"session_token":"first","session_token":"second"}`, `{"upstream_name":"\ud800"}`, `{"agent_grants":[{"agent_id":"a","owner":"o","repo":"r","grant_id":"g","lineage_token":"fixture","expires_unix":18446744073709551616}]}`}
	for index, source := range invalid {
		for _, binary := range []string{rust, goBinary} {
			writeFixture(t, path, source, 0600)
			result := invoke(t, binary, root, []string{"logout"}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path)})
			if result.Code != 1 || !strings.HasPrefix(result.Stderr, "co: invalid config ") {
				t.Fatalf("invalid config case %d accepted or misclassified by %s: %+v", index, binary, result)
			}
			stored, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(stored, []byte(source)) {
				t.Fatal("invalid config was modified", err)
			}
		}
	}
	t.Logf("%d config cases match Rust acceptance and preserve rejected files; %d successful rewrites match JSON semantics", len(sources)+len(invalid), len(sources))
}
