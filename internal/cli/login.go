package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const deviceExpired = "device code expired; run `co login` again"
const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

type loginRuntime struct {
	open func(context.Context, string) error
	wait func(context.Context, time.Duration) error
	now  func() time.Time
}

func login(stdin io.Reader, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return deviceLogin(ctx, stdout, loginRuntime{open: func(ctx context.Context, target string) error { return openBrowser(ctx, target, stdin, stdout, stderr) }, wait: waitForDevice, now: time.Now})
}

func waitForDevice(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func deviceSeconds(value uint64) time.Duration {
	if value > uint64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(value) * time.Second
}

func deviceRecord(response *http.Response, fields []string, prefix string) (configuration, error) {
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || !utf8.Valid(data) || !validConfigUnicode(data) || !json.Valid(data) {
		return nil, fmt.Errorf("%s: invalid JSON", prefix)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(decoder); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON", prefix)
	}
	record, err := configObject(data, fields)
	if err != nil {
		return nil, fmt.Errorf("%s: expected record", prefix)
	}
	return record, nil
}

func optionalDeviceSeconds(record configuration, name string, fallback uint64) (uint64, error) {
	value, present := record[name]
	if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fallback, nil
	}
	seconds, err := strconv.ParseUint(string(value), 10, 64)
	if err != nil {
		return 0, errors.New("invalid API response: invalid device duration")
	}
	return seconds, nil
}

func deviceLogin(ctx context.Context, stdout io.Writer, run loginRuntime) (result error) {
	defer func() {
		if ctx.Err() != nil && result != nil {
			result = errors.New("device authorization cancelled")
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	api := effectiveAPI(config)
	previousAPI := "https://api.co.codes"
	if stored := config.stringValue("api_url"); stored != nil {
		previousAPI = strings.TrimRight(*stored, "/")
	}
	if api != previousAPI {
		for _, name := range []string{"agents", "pending_agent_submissions", "pending_agent_requests", "agent_grants", "machines"} {
			var records []json.RawMessage
			_ = json.Unmarshal(config[name], &records)
			if len(records) > 0 {
				return errors.New("profile contains credentials for another API; use a separate CO_CONFIG_DIR")
			}
		}
	}
	response, err := apiRequest(ctx, api, "POST", "/api/auth/device/code", nil, map[string]string{"client_id": "co-cli", "scope": "openid profile email"})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, err := decodeSessionResponse(response)
		return err
	}
	code, err := deviceRecord(response, []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"}, "invalid API response")
	if err != nil {
		return err
	}
	for _, name := range []string{"device_code", "user_code", "verification_uri"} {
		if err := configField(code, name, "string", "", false); err != nil {
			return errors.New("invalid API response: invalid device code")
		}
	}
	if err := configField(code, "verification_uri_complete", "string", "null", true); err != nil {
		return errors.New("invalid API response: invalid verification URI")
	}
	interval, err := optionalDeviceSeconds(code, "interval", 5)
	if err != nil {
		return err
	}
	expires, err := optionalDeviceSeconds(code, "expires_in", 1800)
	if err != nil {
		return err
	}
	verification := *code.stringValue("verification_uri")
	if !validBrowserURL(verification) {
		return errors.New("invalid API response: invalid verification URI")
	}
	if complete := code.stringValue("verification_uri_complete"); complete != nil && !validBrowserURL(*complete) {
		return errors.New("invalid API response: invalid verification URI")
	}
	deadline := run.now().Add(deviceSeconds(expires))
	if _, err := fmt.Fprintf(stdout, "Open this URL in your browser:\n  %s\nEnter code:\n  %s\n\n", verification, *code.stringValue("user_code")); err != nil {
		return err
	}
	target := verification
	if complete := code.stringValue("verification_uri_complete"); complete != nil {
		target = *complete
	}
	if err := run.open(ctx, target); err != nil {
		if _, err := fmt.Fprintln(stdout, "Unable to open a browser automatically."); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, err := fmt.Fprintln(stdout, "Waiting for approval..."); err != nil {
		return err
	}
	interval = max(interval, 1)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		remaining := deadline.Sub(run.now())
		if remaining <= 0 {
			return errors.New(deviceExpired)
		}
		if err := run.wait(ctx, min(deviceSeconds(interval), remaining)); err != nil {
			return err
		}
		// Do not send a late poll or publish a token after local expiry.
		if !run.now().Before(deadline) {
			return errors.New(deviceExpired)
		}
		response, err = apiRequest(ctx, api, "POST", "/api/auth/device/token", nil, map[string]string{"grant_type": deviceGrant, "device_code": *code.stringValue("device_code"), "client_id": "co-cli"})
		if err != nil {
			return err
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			token, err := deviceRecord(response, []string{"access_token"}, "invalid token response")
			if err != nil {
				return err
			}
			if err := configField(token, "access_token", "string", "", false); err != nil || *token.stringValue("access_token") == "" {
				return errors.New("invalid token response: missing access token")
			}
			if !run.now().Before(deadline) {
				return errors.New(deviceExpired)
			}
			persistCtx, cancel := context.WithTimeout(ctx, deadline.Sub(run.now()))
			err = mutateConfigContext(persistCtx, func(current configuration) error {
				if !sameConfigString(config, current, "session_token") || !sameConfigString(config, current, "api_url") {
					return errors.New("profile changed during authorization; no credentials saved")
				}
				if !run.now().Before(deadline) {
					return errors.New(deviceExpired)
				}
				current["session_token"] = token["access_token"]
				current["api_url"], _ = json.Marshal(api)
				return nil
			})
			cancel()
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return errors.New(deviceExpired)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "Authorized. Run `co whoami` to verify this machine.")
			return err
		}
		poll, err := deviceRecord(response, []string{"error", "error_description", "message"}, "invalid API response")
		if err != nil {
			poll = configuration{"error": json.RawMessage(`"server_error"`)}
		}
		for _, name := range []string{"error", "error_description", "message"} {
			if err := configField(poll, name, "string", "null", true); err != nil {
				poll = configuration{"error": json.RawMessage(`"server_error"`)}
				break
			}
		}
		kind := poll.stringValue("error")
		if kind != nil {
			switch *kind {
			case "authorization_pending":
				continue
			case "slow_down":
				if interval <= math.MaxUint64-5 {
					interval += 5
				}
				continue
			case "access_denied":
				return errors.New("device authorization was denied")
			case "expired_token":
				return errors.New(deviceExpired)
			}
		}
		for _, name := range []string{"error_description", "message", "error"} {
			if text := poll.stringValue(name); text != nil {
				if secret := code.stringValue("device_code"); secret != nil && *secret != "" && strings.Contains(*text, *secret) {
					return errors.New("device authorization failed")
				}
				return errors.New(*text)
			}
		}
		return errors.New("API request failed")
	}
}

func sameConfigString(left, right configuration, name string) bool {
	a, b := left.stringValue(name), right.stringValue(name)
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
