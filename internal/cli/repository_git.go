package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

type repositoryGitCommand struct {
	Spec, Upstream string
	Directory      *string
	JJ             bool
}

func parseRepositoryGit(args []string, clone bool, config configuration) (repositoryGitCommand, error) {
	command := repositoryGitCommand{Upstream: "origin"}
	if upstream := config.stringValue("upstream_name"); upstream != nil {
		command.Upstream = *upstream
	}
	command.JJ = string(config["jj"]) == "true"
	name, usage := "link", "usage: co link [-u <name>] [--jj] <owner/repo>"
	if clone {
		name, usage = "clone", "usage: co clone [-u <name>] [--jj] <owner/repo> [directory]"
	}
	options := true
	var positionals []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case options && arg == "--":
			options = false
		case options && (arg == "--jj" || arg == "--no-jj"):
			command.JJ = arg == "--jj"
		case options && (arg == "-u" || arg == "--set-upstream-name"):
			index++
			if index == len(args) {
				return command, fmt.Errorf("%s requires a remote name\n%s", arg, usage)
			}
			command.Upstream = args[index]
		case options && strings.HasPrefix(arg, "--set-upstream-name="):
			command.Upstream = strings.TrimPrefix(arg, "--set-upstream-name=")
		case options && strings.HasPrefix(arg, "-"):
			return command, fmt.Errorf("unknown %s option %q\n%s", name, arg, usage)
		default:
			positionals = append(positionals, arg)
		}
	}
	maximum := 1
	if clone {
		maximum = 2
	}
	if len(positionals) == 0 || len(positionals) > maximum {
		return command, errors.New(usage)
	}
	if _, _, err := repositorySpec(positionals[0]); err != nil {
		return command, err
	}
	if err := validateUpstreamName(command.Upstream); err != nil {
		return command, err
	}
	command.Spec = positionals[0]
	if len(positionals) > 1 {
		command.Directory = &positionals[1]
	}
	return command, nil
}

func validateUpstreamName(name string) error {
	invalid := fmt.Errorf("%q is not a valid Git remote name", name)
	if name == "" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, "/") {
		return invalid
	}
	for _, part := range []string{"..", "@{", "//"} {
		if strings.Contains(name, part) {
			return invalid
		}
	}
	for _, character := range name {
		if unicode.IsControl(character) || strings.ContainsRune(" ~^:?*[\\", character) {
			return invalid
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return invalid
		}
	}
	return nil
}

func repositoryGit(args []string, clone bool, stdin io.Reader, stdout, stderr io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	command, err := parseRepositoryGit(args, clone, config)
	if err != nil {
		return err
	}
	executable, err := credentialHelperExecutable()
	if err != nil {
		return err
	}
	if !utf8.ValidString(executable) {
		return errors.New("the co executable path is not valid UTF-8")
	}
	if runtime.GOOS == "windows" {
		executable = windowsGitShellPath(executable)
	}
	helper := "!'" + strings.ReplaceAll(executable, "'", "'\\''") + "' git-credential"
	host := gitHostForAPI(effectiveAPI(config))
	url := "https://" + host + "/" + command.Spec + ".git"
	run := func(args []string, action string) error {
		child, err := repositoryProcess("git", args)
		if err != nil {
			return fmt.Errorf("unable to run Git to %s: %s", action, processError(err))
		}
		child.Stdin, child.Stdout, child.Stderr = stdin, stdout, stderr
		if err := child.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("Git failed to %s (%s)", action, processStatus(exit.ProcessState))
			}
			return fmt.Errorf("unable to run Git to %s: %s", action, processError(err))
		}
		return nil
	}
	var directory string
	if clone {
		args := []string{"-c", "credential.helper=", "-c", "credential.https://" + host + ".helper=" + helper, "-c", "credential.https://" + host + ".useHttpPath=true", "clone", "--origin", command.Upstream, "--", url}
		if command.Directory != nil {
			directory = *command.Directory
			args = append(args, directory)
		} else {
			_, directory, _ = repositorySpec(command.Spec)
		}
		if err := run(args, "clone repository"); err != nil {
			return err
		}
	} else {
		directory, err = gitRepositoryRoot()
		if err != nil {
			return err
		}
		if err := run([]string{"-C", directory, "remote", "add", command.Upstream, url}, "link repository"); err != nil {
			return err
		}
	}
	if !utf8.ValidString(directory) {
		return errors.New("the clone directory is not valid UTF-8")
	}
	key := "credential.https://" + host
	for _, step := range []struct {
		args   []string
		action string
	}{
		{[]string{"--replace-all", key + ".helper", ""}, "reset repository credential helpers"},
		{[]string{"--add", key + ".helper", helper}, "configure repository credential helper"},
		{[]string{key + ".useHttpPath", "true"}, "configure repository-scoped credentials"},
	} {
		if err := run(append([]string{"-C", directory, "config", "--local"}, step.args...), step.action); err != nil {
			return err
		}
	}
	if command.JJ {
		if _, err := os.Stat(filepath.Join(directory, ".jj")); err == nil {
			child, err := repositoryProcess("jj", []string{"--repository", directory, "root"})
			if err != nil {
				return fmt.Errorf("unable to inspect existing jj repository: %s", processError(err))
			}
			if err := child.Run(); err == nil {
				return nil
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					return fmt.Errorf("unable to inspect existing jj repository: %s", processError(err))
				}
			}
		}
		child, err := repositoryProcess("jj", []string{"git", "init", "--colocate"})
		if err != nil {
			return fmt.Errorf("unable to initialize jj: %s", processError(err))
		}
		child.Dir, child.Stdin, child.Stdout, child.Stderr = directory, stdin, stdout, stderr
		if err := child.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("jj failed to initialize a colocated repository (%s)", processStatus(exit.ProcessState))
			}
			return fmt.Errorf("unable to initialize jj: %s", processError(err))
		}
	}
	return nil
}

func repositoryProcess(name string, args []string) (*exec.Cmd, error) {
	path, err := lookupExecutable(name)
	if err != nil {
		return nil, err
	}
	return &exec.Cmd{Path: path, Args: append([]string{name}, args...)}, nil
}

func gitRepositoryRoot() (string, error) {
	inspect := func(arg string) ([]byte, error) {
		child, err := repositoryProcess("git", []string{"rev-parse", arg})
		if err != nil {
			return nil, fmt.Errorf("unable to locate the Git repository: %s", processError(err))
		}
		data, err := child.Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return nil, errors.New("the current directory is not inside a Git repository")
			}
			return nil, fmt.Errorf("unable to locate the Git repository: %s", processError(err))
		}
		return data, nil
	}
	bare, err := inspect("--is-bare-repository")
	if err != nil {
		return "", err
	}
	arg := "--show-toplevel"
	if strings.HasPrefix(string(bare), "true") {
		arg = "--absolute-git-dir"
	}
	root, err := inspect(arg)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(root) {
		return "", errors.New("the Git repository path is not valid UTF-8")
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(root), "\n"), "\r"), nil
}

func credentialHelperExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("unable to locate the co executable: %s", processError(err))
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		resolved = executable
	}
	if len(os.Args) == 0 {
		return executable, nil
	}
	invoked := os.Args[0]
	if runtime.GOOS == "windows" && filepath.Ext(invoked) == "" {
		invoked += ".exe"
	}
	candidates := []string{invoked}
	if filepath.Base(invoked) == invoked {
		candidates = nil
		directories := filepath.SplitList(os.Getenv("PATH"))
		if len(directories) == 0 {
			directories = []string{""}
		}
		for _, directory := range directories {
			candidates = append(candidates, filepath.Join(directory, invoked))
		}
	}
	for _, candidate := range candidates {
		target, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		absolute, err := filepath.Abs(target)
		if err == nil && absolute == resolved {
			return filepath.Abs(candidate)
		}
	}
	return executable, nil
}

func windowsGitShellPath(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return "//" + strings.ReplaceAll(strings.TrimPrefix(path, `\\?\UNC\`), `\`, "/")
	}
	return strings.ReplaceAll(strings.TrimPrefix(path, `\\?\`), `\`, "/")
}
