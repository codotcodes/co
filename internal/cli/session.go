package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func sessionRequest(config configuration, method, path, token string) (*http.Response, error) {
	return apiRequest(context.Background(), effectiveAPI(config), method, path, &token, nil)
}

func apiRequest(ctx context.Context, api, method, path string, token *string, body any) (*http.Response, error) {
	var credential *apiCredential
	if token != nil {
		credential = &apiCredential{scheme: "Bearer", secret: *token}
	}
	return requestAPI(ctx, api, method, path, credential, body)
}

type apiCredential struct {
	scheme, secret string
	scoped         bool
}

func requestAPI(ctx context.Context, api, method, path string, credential *apiCredential, body any) (*http.Response, error) {
	return requestAPIHeaders(ctx, api, method, path, credential, body, nil)
}

func requestAPIHeaders(ctx context.Context, api, method, path string, credential *apiCredential, body any, headers map[string]string) (*http.Response, error) {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, api+path, input)
	if err != nil {
		return nil, errors.New("network request failed: invalid API URL")
	}
	if credential != nil && credential.scheme != "" {
		request.Header.Set("Authorization", credential.scheme+" "+credential.secret)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", "co/"+Version)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if credential != nil && credential.scoped {
			return errors.New("scoped credential redirect refused")
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if body != nil && !sameAPIOrigin(next.URL, via[0].URL) {
			return errors.New("credential payload redirect refused")
		}
		if !sameAPIOrigin(next.URL, via[0].URL) || via[len(via)-1].Header.Get("Authorization") == "" {
			next.Header.Del("Authorization")
			next.Header.Del("Cookie")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		var timed interface{ Timeout() bool }
		if errors.As(err, &timed) && timed.Timeout() {
			return nil, errors.New("request timed out")
		}
		// Do not include a URL or headers that may contain credential material.
		return nil, errors.New("network request failed")
	}
	return response, nil
}

func sameAPIOrigin(left, right *url.URL) bool {
	port := func(value *url.URL) string {
		if value.Port() != "" {
			return value.Port()
		}
		if value.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Hostname(), right.Hostname()) && port(left) == port(right)
}

func decodeSessionResponse(response *http.Response) (any, error) {
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var body any
	err := decoder.Decode(&body)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if err != nil {
			return nil, errors.New("invalid API response: invalid JSON")
		}
		return body, nil
	}
	if object, ok := body.(map[string]any); err == nil && ok {
		valid := true
		for _, name := range []string{"error_description", "message", "error"} {
			if value := object[name]; value != nil {
				if _, ok := value.(string); !ok {
					valid = false
				}
			}
		}
		if valid {
			for _, name := range []string{"error_description", "message", "error"} {
				if value, ok := object[name].(string); ok {
					return nil, errors.New(value)
				}
			}
			return nil, errors.New("API request failed")
		}
	}
	return nil, fmt.Errorf("HTTP %d %s", response.StatusCode, http.StatusText(response.StatusCode))
}

func whoami(stdout io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	token := config.stringValue("session_token")
	if token == nil {
		return errors.New("not signed in; run `co login`")
	}
	response, err := sessionRequest(config, http.MethodGet, "/api/auth/get-session", *token)
	if err != nil {
		return err
	}
	body, err := decodeSessionResponse(response)
	if err != nil {
		return err
	}
	object, _ := body.(map[string]any)
	user, exists := object["user"]
	if !exists {
		return errors.New("session is no longer valid; run `co login`")
	}
	fields, _ := user.(map[string]any)
	name, email := "unknown", "unknown"
	if value, ok := fields["name"].(string); ok {
		name = value
	}
	if value, ok := fields["email"].(string); ok {
		email = value
	}
	username := name
	if value, ok := fields["username"].(string); ok {
		username = value
	}
	_, err = fmt.Fprintf(stdout, "%s\n  name   %s\n  email  %s\n", username, name, email)
	return err
}

func logout(stdout, stderr io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	token := config.stringValue("session_token")
	if token != nil {
		response, err := sessionRequest(config, http.MethodPost, "/api/auth/sign-out", *token)
		if err != nil {
			fmt.Fprintln(stderr, "co: warning: unable to reach the server; removing the local session")
		} else {
			response.Body.Close()
			if !(response.StatusCode >= 200 && response.StatusCode < 300) && response.StatusCode != 401 {
				fmt.Fprintln(stderr, "co: warning: server did not confirm session revocation")
			}
		}
	}
	err = mutateConfig(func(current configuration) error {
		fresh := current.stringValue("session_token")
		sameSession := token == nil && fresh == nil || token != nil && fresh != nil && *token == *fresh
		if sameSession && effectiveAPI(config) == effectiveAPI(current) {
			current["session_token"] = json.RawMessage("null")
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "Signed out.")
	return err
}
