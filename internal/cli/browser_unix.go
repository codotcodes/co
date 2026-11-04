//go:build !windows && !darwin

package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"
)

func platformBrowser(ctx context.Context, target string, stdin io.Reader, stdout, stderr io.Writer) error {
	try := func(args []string, terminal bool) bool {
		return browserCommand(ctx, args, terminal, stdin, stdout, stderr) == nil
	}
	for _, spec := range strings.Split(os.Getenv("BROWSER"), ":") {
		args := browserArguments(spec, target)
		if len(args) == 0 {
			continue
		}
		terminal := slices.Contains([]string{"lynx", "links", "links2", "elinks", "w3m", "eww", "netrik", "retawq", "curl"}, filepath.Base(args[0]))
		if try(args, terminal) {
			return nil
		}
	}
	if args, terminal := xdgBrowser(ctx, target); len(args) > 0 && try(args, terminal) {
		return nil
	}
	desktop := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP") + " " + os.Getenv("DESKTOP_SESSION"))
	_, kdeFull := os.LookupEnv("KDE_FULL_SESSION")
	_, kdeVersion := os.LookupEnv("KDE_SESSION_VERSION")
	var candidates [][]string
	switch {
	case strings.Contains(desktop, "gnome"), strings.Contains(desktop, "cinnamon"):
		candidates = [][]string{{"gio", "open", target}, {"gvfs-open", target}, {"gnome-open", target}}
	case strings.Contains(desktop, "kde"), kdeFull, kdeVersion:
		candidates = [][]string{{"kde-open", target}, {"kde-open5", target}, {"kfmclient", "newTab", target}}
	case strings.Contains(desktop, "mate"):
		candidates = [][]string{{"gio", "open", target}, {"gvfs-open", target}, {"mate-open", target}}
	case strings.Contains(desktop, "xfce"):
		candidates = [][]string{{"exo-open", target}, {"gio", "open", target}, {"gvfs-open", target}}
	}
	if strings.EqualFold(os.Getenv("container"), "flatpak") {
		candidates = [][]string{{"xdg-open", target}}
	}
	if data, err := os.ReadFile("/proc/sys/fs/binfmt_misc/WSLInterop"); len(candidates) == 0 && err == nil && strings.Contains(string(data), "enabled") {
		candidates = [][]string{wslBrowserCommand(target), {"wsl-open", target}}
	}
	candidates = append(candidates, []string{"x-www-browser", target})
	for _, args := range candidates {
		if try(args, false) {
			return nil
		}
	}
	return errors.New("no valid browser available")
}

func xdgBrowser(ctx context.Context, target string) ([]string, bool) {
	command := exec.CommandContext(ctx, "xdg-settings", "get", "default-web-browser")
	data, err := command.Output()
	if err != nil {
		return nil, false
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return nil, false
	}
	var directories []string
	if home := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(home) {
		directories = append(directories, home)
	} else if home, err := os.UserHomeDir(); err == nil {
		directories = append(directories, filepath.Join(home, ".local", "share"))
	}
	if dirs, present := os.LookupEnv("XDG_DATA_DIRS"); present {
		directories = append(directories, strings.Split(dirs, ":")...)
	} else {
		directories = append(directories, "/usr/local/share", "/usr/share")
	}
	for _, directory := range directories {
		path := filepath.Join(directory, "applications", name)
		if _, err := os.Stat(path); err != nil && strings.Contains(name, "-") {
			path = filepath.Join(directory, "applications", strings.ReplaceAll(name, "-", "/"))
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		entry, hidden, terminal := false, false, false
		line := ""
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "[") {
				entry = text == "[Desktop Entry]"
				continue
			}
			if !entry || strings.HasPrefix(text, "#") {
				continue
			}
			key, value, ok := strings.Cut(text, "=")
			if !ok {
				continue
			}
			switch key {
			case "Exec":
				line = value
			case "Hidden":
				hidden = value == "true"
			case "Terminal":
				terminal = value == "true"
			}
		}
		file.Close()
		if hidden || line == "" {
			continue
		}
		args := asciiFields(line)
		used := false
		for index := 1; index < len(args); index++ {
			if slices.Contains([]string{"%u", "%U", "%f", "%F"}, args[index]) {
				args[index] = target
				used = true
			}
		}
		if len(args) == 0 {
			continue
		}
		if _, err := exec.LookPath(args[0]); err != nil && !errors.Is(err, exec.ErrDot) {
			continue
		}
		if !used {
			args = append(args, target)
		}
		return args, terminal
	}
	return nil, false
}

func wslBrowserCommand(target string) []string {
	// Do not interpolate a server-supplied URL into cmd.exe or PowerShell syntax.
	script := "Start-Process -FilePath '" + strings.ReplaceAll(target, "'", "''") + "'"
	units := utf16.Encode([]rune(script))
	data := make([]byte, 2*len(units))
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[index*2:], unit)
	}
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(data)}
}
