//go:build !windows && !darwin

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBrowserFieldCodesAndXDGSelection(t *testing.T) {
	target := "https://fixture.invalid/verify?code=USER%25s"
	for _, test := range []struct {
		spec string
		want []string
	}{
		{"opener --url=%s%% --colon=%c", []string{"opener", "--url=" + target + "%", "--colon=:"}},
		{"opener %%s", []string{"opener", "%s", target}},
		{"  ", nil},
	} {
		if got := browserArguments(test.spec, target); !reflect.DeepEqual(got, test.want) {
			t.Fatal("browser expansion differs", got, test.want)
		}
	}
	root := t.TempDir()
	log := filepath.Join(root, "opened")
	t.Setenv("BROWSER", "")
	t.Setenv("PATH", root)
	t.Setenv("XDG_DATA_HOME", root)
	t.Setenv("XDG_DATA_DIRS", root)
	t.Setenv("XDG_CURRENT_DESKTOP", "")
	t.Setenv("DESKTOP_SESSION", "")
	writeFixture(t, filepath.Join(root, "xdg-settings"), "#!/bin/sh\nprintf 'fixture.desktop\\n'\n", 0500)
	writeFixture(t, filepath.Join(root, "browser"), "#!/bin/sh\nprintf '%s' \"$1\" > \""+log+"\"\n", 0500)
	writeFixture(t, filepath.Join(root, "applications", "fixture.desktop"), "[Desktop Entry]\nExec="+filepath.Join(root, "browser")+" %u\nTerminal=true\n", 0600)
	if err := openBrowser(t.Context(), target, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil || string(data) != target {
		t.Fatal("XDG browser handoff failed", err)
	}
	for _, unsafe := range []string{"file:///tmp/fixture", "javascript:alert(1)", "https://user:credential@fixture.invalid/"} {
		if err := openBrowser(t.Context(), unsafe, nil, io.Discard, io.Discard); err == nil {
			t.Fatal("unsafe verification URL opened")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := browserCommand(ctx, []string{filepath.Join(root, "browser"), target}, true, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("canceled browser launch succeeded")
	}
}
