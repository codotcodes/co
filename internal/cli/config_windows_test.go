package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsConfigRewritesKeepOwnerOnlyACLs(t *testing.T) {
	binary := goBinaryForTests(t)
	root := t.TempDir()
	profile := filepath.Join(root, "café credentials")
	command := func() *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), binary, "logout")
		cmd.Env = append(os.Environ(), "CO_CONFIG_DIR="+profile)
		return cmd
	}
	if output, err := command().CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	processes := make([]*exec.Cmd, 12)
	outputs := make([]bytes.Buffer, len(processes))
	for index := range processes {
		cmd := command()
		cmd.Stdout, cmd.Stderr = &outputs[index], &outputs[index]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		processes[index] = cmd
	}
	for index, process := range processes {
		if err := process.Wait(); err != nil {
			t.Fatal("concurrent logout failed", err, outputs[index].String())
		}
	}
	data, err := os.ReadFile(filepath.Join(profile, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil || config["session_token"] != nil {
		t.Fatal("invalid logged-out profile", err)
	}
	entries, err := os.ReadDir(profile)
	if err != nil || len(entries) != 2 {
		t.Fatal("unexpected credential temporary files", err, entries)
	}
	probe := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `
$ErrorActionPreference = 'Stop'
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
foreach ($path in @($env:CO_CONFIG_DIR, "$env:CO_CONFIG_DIR\config.json", "$env:CO_CONFIG_DIR\config.json.lock")) {
    $acl = Get-Acl -LiteralPath $path
    if (!$acl.AreAccessRulesProtected) { throw 'Inherited credential ACL' }
    if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -ne $sid) { throw 'Wrong owner' }
    $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    if ($rules.Count -ne 1 -or $rules[0].IdentityReference.Value -ne $sid -or $rules[0].AccessControlType -ne 'Allow' -or $rules[0].FileSystemRights -ne 'FullControl') { throw 'Credential ACL grants another identity' }
}
`)
	for _, entry := range os.Environ() {
		// Windows PowerShell cannot load the Security module from PowerShell 7.
		if !strings.EqualFold(strings.SplitN(entry, "=", 2)[0], "PSModulePath") && !strings.HasPrefix(strings.ToUpper(entry), "CO_CONFIG_DIR=") {
			probe.Env = append(probe.Env, entry)
		}
	}
	probe.Env = append(probe.Env, "CO_CONFIG_DIR="+profile)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatal("credential ACL probe failed", err, string(output))
	}
}
