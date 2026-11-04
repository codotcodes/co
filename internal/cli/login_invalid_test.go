package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompiledInvalidDeviceCodeResponses(t *testing.T) {
	rust, goBinary := comparisonBinaries(t)
	for _, body := range []string{`null`, `[]`, `not JSON`, `{"device_code":null}`, `{"device_code":"fixture","user_code":"USER","verification_uri":"https://fixture.invalid","interval":-1}`, `{"device_code":"first","device_code":"second","user_code":"USER","verification_uri":"https://fixture.invalid"}`} {
		root := t.TempDir()
		path := filepath.Join(root, "profile", "config.json")
		var polls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/api/auth/device/code" {
				polls.Add(1)
			}
			io.WriteString(writer, body)
		}))
		for _, binary := range []string{rust, goBinary} {
			writeConfigFixture(t, path, fullConfig(server.URL))
			before, _ := os.ReadFile(path)
			result := invoke(t, binary, root, []string{"login"}, []string{"CO_CONFIG_DIR=" + filepath.Dir(path), "CO_API_URL=" + server.URL, "PATH=" + root, "BROWSER=missing"})
			if result.Code != 1 || !strings.HasPrefix(result.Stderr, "co: invalid API response:") || result.Stdout != "" {
				t.Fatal("malformed device response accepted or exposed", result)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("malformed response changed credentials")
			}
		}
		server.Close()
		if polls.Load() != 0 {
			t.Fatal("invalid device response reached token polling")
		}
	}
	t.Log("6 malformed device-code responses match Rust rejection before browser handoff, polling or credential writes")
}
