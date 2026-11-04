package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func doctor(stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := loadConfig()
	if err != nil {
		return err
	}
	api := effectiveAPI(config)
	response, err := requestAPI(ctx, api, "GET", "/health", nil, nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("API health check returned %d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "api       ok (%s)\nconfig    %s\n", api, path); err != nil {
		return err
	}
	status := "absent (run `co login`)"
	if config.stringValue("session_token") != nil {
		status = "invalid (run `co login`)"
		if credential, err := agentHumanCredential(config); err == nil {
			if session, err := requestAPI(ctx, api, "GET", "/api/auth/get-session", credential, nil); err == nil {
				session.Body.Close()
				if session.StatusCode >= 200 && session.StatusCode < 300 {
					status = "valid"
				}
			}
		}
	}
	if _, err := fmt.Fprintln(stdout, "session   "+status); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	git, err := repositoryProcess("git", []string{"--version"})
	if err != nil {
		return fmt.Errorf("unable to run Git: %s", processError(err))
	}
	if err := git.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("Git health check failed (%s)", processStatus(exit.ProcessState))
		}
		return fmt.Errorf("unable to run Git: %s", processError(err))
	}
	_, err = fmt.Fprintln(stdout, "git       ok (HTTPS credential helper ready)")
	return err
}
