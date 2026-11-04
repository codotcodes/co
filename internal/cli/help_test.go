package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCompiledTopLevelArgumentsMatchRust(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	root := t.TempDir()
	profile := filepath.Join(root, "profile", "config.json")
	for _, args := range [][]string{
		nil, {"help"}, {"--help"}, {"-h"}, {"help", "extra"}, {"version"}, {"--version"}, {"-V"}, {"version", "extra"}, {"unknown"},
		{"repo"}, {"repo", "view"}, {"repo", "create", "--help"}, {"repo", "create", "-h"},
		{"agent"}, {"agent", "register"}, {"agent", "register", "one", "extra"}, {"agent", "list", "extra"}, {"agent", "attest"},
		{"access"}, {"access", "unknown"}, {"access", "wait", "one", "extra"}, {"access", "view"}, {"access", "view", "one", "extra"},
		{"machine"}, {"machine", "run"}, {"machine", "run", "one", "extra"}, {"machine", "heartbeat"}, {"machine", "list", "extra"},
		{"git-credential"}, {"git-credential", "unknown"}, {"git-credential", "get", "extra"},
		{"secret"}, {"secret", "help"}, {"secret", "set", "help"}, {"deployment"}, {"deployment", "help"},
	} {
		writeConfigFixture(t, profile, configuration{})
		env := []string{"CO_CONFIG_DIR=" + filepath.Dir(profile)}
		want := invoke(t, rust, root, args, env)
		writeConfigFixture(t, profile, configuration{})
		actual := invoke(t, goBinary, root, args, env)
		if !reflect.DeepEqual(actual, want) {
			t.Errorf("top-level arguments %q differ: Go=%+v Rust=%+v", args, actual, want)
		}
		if info, err := os.Stat(profile); err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("argument sweep changed profile permissions", err)
		}
	}
	t.Log("37 compiled top-level help/version/usage/arity cases match retained Rust without a fallback path")
}
