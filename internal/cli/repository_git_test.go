package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestCompiledCloneLinkRustParity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux executable fixtures")
	}
	rust, goBinary := comparisonBinaries(t)
	for _, scenario := range []string{"clone", "directory", "options", "terminator", "staging", "staging443", "custom-api", "link", "bare", "jj", "existing-jj", "broken-jj", "git-failure", "config-failure", "jj-failure", "missing-git", "missing-jj", "not-repo", "invalid-remote", "missing-remote", "unknown", "extra", "no-spec", "bad-spec"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile", "config.json")
			log := filepath.Join(root, "commands")
			bin := filepath.Join(root, "bin")
			gitScript := `#!/bin/sh
printf '%s\000' '--git--' "$@" >> "$CO_COMMAND_LOG"
if [ "$1" = rev-parse ]; then
  if [ "$CO_SCENARIO" = not-repo ]; then exit 128; fi
  if [ "$2" = --is-bare-repository ]; then
    if [ "$CO_SCENARIO" = bare ]; then printf 'true\n'; else printf 'false\n'; fi
  else printf '%s\n' "$CO_FIXTURE_ROOT"; fi
  exit 0
fi
if [ "$CO_SCENARIO" = git-failure ]; then exit 7; fi
if [ "$CO_SCENARIO" = config-failure ] && [ "$3" = config ]; then exit 9; fi
printf 'git stdout\n'
printf 'git stderr\n' >&2
`
			jjScript := `#!/bin/sh
printf '%s\000' '--jj--' "$@" >> "$CO_COMMAND_LOG"
if [ "$1" = --repository ]; then
  if [ "$CO_SCENARIO" = broken-jj ]; then exit 2; fi
  exit 0
fi
if [ "$CO_SCENARIO" = jj-failure ]; then exit 8; fi
printf 'jj stdout\n'
printf 'jj stderr\n' >&2
`
			if scenario != "missing-git" {
				writeFixture(t, filepath.Join(bin, "git"), gitScript, 0500)
			}
			if scenario != "missing-jj" {
				writeFixture(t, filepath.Join(bin, "jj"), jjScript, 0500)
			}
			config := fullConfig("https://api.co.codes")
			config["jj"] = json.RawMessage(`false`)
			config["upstream_name"] = json.RawMessage(`null`)
			args := []string{"clone", "owner/repo"}
			switch scenario {
			case "directory":
				args = append(args, "space 日本語")
			case "options":
				args = []string{"clone", "--jj", "--no-jj", "--set-upstream-name=upstream", "owner/repo"}
			case "terminator":
				args = []string{"clone", "--", "owner/repo", "--directory"}
			case "staging":
				config["api_url"] = json.RawMessage(`"https://api.codevved.com"`)
			case "staging443":
				config["api_url"] = json.RawMessage(`"https://api.codevved.com:443/"`)
			case "custom-api":
				config["api_url"] = json.RawMessage(`"https://unrelated.invalid"`)
			case "link", "bare", "not-repo":
				args = []string{"link", "-u", "upstream", "owner/repo"}
			case "jj", "existing-jj", "broken-jj", "jj-failure", "missing-jj":
				args = append(args, "--jj")
				if scenario == "existing-jj" || scenario == "broken-jj" {
					if err := os.MkdirAll(filepath.Join(root, "repo", ".jj"), 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.MkdirAll(filepath.Join(root, "repo"), 0700); err != nil {
					t.Fatal(err)
				}
			case "invalid-remote":
				args = append(args, "-u", "bad..name")
			case "missing-remote":
				args = append(args, "-u")
			case "unknown":
				args = append(args, "--unknown")
			case "extra":
				args = append(args, "one", "two")
			case "no-spec":
				args = args[:1]
			case "bad-spec":
				args[1] = "owner/../repo"
			}
			writeConfigFixture(t, path, config)
			before, _ := os.ReadFile(path)
			var expected observation
			var expectedCommands string
			for index, binary := range []string{rust, goBinary} {
				_ = os.Remove(log)
				result := invoke(t, binary, root, args, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + strings.Trim(string(config["api_url"]), `"`), "PATH=" + bin, "CO_COMMAND_LOG=" + log, "CO_SCENARIO=" + scenario, "CO_FIXTURE_ROOT=" + root})
				commands, _ := os.ReadFile(log)
				normalized := strings.ReplaceAll(string(commands), binary, "<co-executable>")
				if index == 0 {
					expected, expectedCommands = result, normalized
				} else if !reflect.DeepEqual(result, expected) || normalized != expectedCommands {
					t.Fatalf("clone/link differs: Go=%+v Rust=%+v commands=%q/%q", result, expected, normalized, expectedCommands)
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) || strings.Contains(normalized+result.Stdout+result.Stderr, "fixture-human-session") || strings.Contains(normalized, "fixture-lineage") {
					t.Fatal("clone/link changed or exposed profile credentials")
				}
			}
		})
	}
	t.Log("24 compiled clone/link cases match Rust child arguments, streams and failures with no credentials in child arguments")
}

func TestRealGitCloneLinkAndStableHelperSymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux symlink and real Git fixture")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for name, binary := range contractBinaries(t) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			runGit := func(directory string, args ...string) string {
				t.Helper()
				command := exec.Command(git, args...)
				command.Dir = directory
				command.Env = []string{"HOME=" + root, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(root, "global"), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
				out, err := command.CombinedOutput()
				if err != nil {
					t.Fatal(args, err, string(out))
				}
				return string(out)
			}
			source := filepath.Join(root, "source")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			runGit(source, "init", "--initial-branch=main")
			writeFixture(t, filepath.Join(source, "file"), "fixture bytes\n", 0600)
			runGit(source, "add", "file")
			runGit(source, "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
			global := filepath.Join(root, "global")
			writeFixture(t, global, "[url \"file://"+source+"\"]\n\tinsteadOf = https://git.co.codes/owner/repo.git\n[credential]\n\thelper = !false\n", 0600)
			profile := filepath.Join(root, "profile", "config.json")
			config := fullConfig("https://api.co.codes")
			config["jj"] = json.RawMessage(`false`)
			writeConfigFixture(t, profile, config)
			alias := filepath.Join(root, "installation 日本語's", "co")
			if err := os.MkdirAll(filepath.Dir(alias), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(binary, alias); err != nil {
				t.Fatal(err)
			}
			env := []string{"CO_CONFIG_DIR=" + filepath.Dir(profile), "CO_API_URL=https://api.co.codes", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + global}
			result := invoke(t, alias, root, []string{"clone", "--no-jj", "-u", "upstream", "owner/repo", "cloned"}, env)
			if result.Code != 0 {
				t.Fatal(result)
			}
			cloned := filepath.Join(root, "cloned")
			if data, _ := os.ReadFile(filepath.Join(cloned, "file")); string(data) != "fixture bytes\n" {
				t.Fatal("clone lost repository content")
			}
			if got := runGit(cloned, "config", "--get", "remote.upstream.url"); got != "https://git.co.codes/owner/repo.git\n" {
				t.Fatal("clone persisted rewritten fixture URL", got)
			}
			wantHelper := "\n!'" + strings.ReplaceAll(alias, "'", "'\\''") + "' git-credential\n"
			if got := runGit(cloned, "config", "--get-all", "credential.https://git.co.codes.helper"); got != wantHelper {
				t.Fatalf("helper is not stable installation symlink: got=%q want=%q", got, wantHelper)
			}
			if got := runGit(cloned, "config", "--get", "credential.https://git.co.codes.useHttpPath"); got != "true\n" {
				t.Fatal("clone omitted repository credential scope")
			}
			result = invoke(t, alias, cloned, []string{"link", "--no-jj", "-u", "second", "owner/other"}, env)
			if result.Code != 0 || runGit(cloned, "config", "--get", "remote.second.url") != "https://git.co.codes/owner/other.git\n" {
				t.Fatal("link failed", result)
			}
			if got := runGit(cloned, "config", "--get-all", "credential.https://git.co.codes.helper"); got != wantHelper {
				t.Fatal("link duplicated inherited helpers", got)
			}
		})
	}
	t.Log("real Git clone/link preserves contents, remotes, helper resets, path scoping and installation symlinks; URL rewrite keeps all traffic local")
}

func TestRepositoryRemoteAndWindowsShellPaths(t *testing.T) {
	for _, name := range []string{"", "-bad", "bad.", "bad/", "bad..name", "bad@{name", "a//b", "a b", "a\nb", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", ".hidden", "a/.hidden", "a.lock", "a.lock/b"} {
		if err := validateUpstreamName(name); err == nil {
			t.Fatal("invalid remote accepted", name)
		}
	}
	for _, name := range []string{"origin", "upstream", "team/branch", "日本語", "a_b", "a-b"} {
		if err := validateUpstreamName(name); err != nil {
			t.Fatal("valid remote rejected", name, err)
		}
	}
	for _, test := range []struct{ input, want string }{
		{`C:\Program Files\co.exe`, "C:/Program Files/co.exe"},
		{`\\?\C:\co.exe`, "C:/co.exe"},
		{`\\?\UNC\server\share\co.exe`, "//server/share/co.exe"},
		{`\\server\share\co.exe`, "//server/share/co.exe"},
	} {
		if got := windowsGitShellPath(test.input); got != test.want {
			t.Fatal(test, got)
		}
	}
}
