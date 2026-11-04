package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

const maxSecretBytes = 16_384

func validateSecretName(name string) error {
	if len(name) == 0 || len(name) > 100 {
		return errors.New("secret name must be 1 to 100 characters")
	}
	if !(name[0] >= 'A' && name[0] <= 'Z' || name[0] == '_') {
		return errors.New("secret name must start with an uppercase letter or underscore")
	}
	for _, value := range []byte(name) {
		if !(value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_') {
			return errors.New("secret name must contain only uppercase letters, digits, and underscores")
		}
	}
	return nil
}

func validateSecretValue(value string) error {
	if len(value) > maxSecretBytes {
		return fmt.Errorf("secret value exceeds %d UTF-8 bytes", maxSecretBytes)
	}
	if strings.ContainsRune(value, 0) {
		return errors.New("secret value must not contain a NUL byte")
	}
	return nil
}

func readBoundedSecret(ctx context.Context, input io.Reader, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var data []byte
	var err error
	if file, ok := input.(*os.File); ok {
		data, err = readSecretFileContext(ctx, file, limit)
	} else {
		data, err = io.ReadAll(io.LimitReader(input, int64(limit+1)))
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || !utf8.Valid(data) {
		return "", errors.New("unable to read UTF-8 secret input")
	}
	if len(data) > limit {
		return "", fmt.Errorf("secret input exceeds %d UTF-8 bytes", limit)
	}
	return string(data), nil
}

func readSecretValue(ctx context.Context, input io.Reader, path *string) (string, error) {
	if path != nil {
		file, err := os.Open(*path)
		if err != nil {
			return "", errors.New("unable to open secret value file")
		}
		defer file.Close()
		return readBoundedSecret(ctx, file, maxSecretBytes)
	}
	file, isFile := input.(*os.File)
	isTerminal := false
	if isFile {
		if descriptor, err := secretFileDescriptor(file); err == nil {
			isTerminal = term.IsTerminal(descriptor)
		}
	}
	if isTerminal {
		value, err := readHiddenSecret(ctx, "Enter the secret value: ", maxSecretBytes)
		if err != nil {
			return "", errors.New("unable to read secret value")
		}
		return value, nil
	}
	return readBoundedSecret(ctx, input, maxSecretBytes)
}

func secretFileDescriptor(file *os.File) (int, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var descriptor int
	err = connection.Control(func(fd uintptr) { descriptor = int(fd) })
	return descriptor, err
}

func selectReauthentication(methods []string) (string, error) {
	for _, desired := range []string{"password", "totp"} {
		for _, method := range methods {
			if method == desired {
				return desired, nil
			}
		}
	}
	for _, method := range methods {
		if method == "passkey" {
			return "", errors.New("this account only supports passkey reauthentication, which requires browser-based WebAuthn and is not available in the terminal CLI. Create the secret in the web UI, or enroll an authenticator to use TOTP.")
		}
	}
	return "", errors.New("no reauthentication method is available for this account. Enroll a passkey or authenticator in your account settings, then try again.")
}

func reauthenticationProof(ctx context.Context, method string, path *string) (map[string]any, error) {
	key, prompt := "password", "Enter your account password: "
	if method == "totp" {
		key, prompt = "code", "Enter your 6-digit authentication code: "
	} else if method != "password" {
		return nil, fmt.Errorf("unsupported reauthentication method %q", method)
	}
	var value string
	var err error
	if path != nil {
		file, openErr := os.Open(*path)
		if openErr != nil {
			return nil, errors.New("unable to open reauthentication file")
		}
		defer file.Close()
		value, err = readBoundedSecret(ctx, file, 4096)
		if strings.HasSuffix(value, "\n") {
			value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
		}
	} else {
		value, err = readHiddenSecret(ctx, prompt, 4096)
		if err != nil {
			return nil, errors.New("unable to read authentication from the terminal; use --reauth-file for noninteractive input")
		}
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{key: value}, nil
}

func readHiddenSecret(ctx context.Context, prompt string, limit int) (string, error) {
	input, output, descriptor, cleanup, err := openSecretTerminal()
	if err != nil {
		return "", err
	}
	defer cleanup()
	state, err := term.MakeRaw(descriptor)
	if err != nil {
		return "", err
	}
	restored := false
	defer func() {
		if !restored {
			_ = term.Restore(descriptor, state)
		}
	}()
	if _, err := io.WriteString(output, prompt); err != nil {
		return "", err
	}
	type result struct {
		value string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := readHiddenLine(input, limit)
		done <- result{value, err}
	}()
	select {
	case value := <-done:
		_, _ = io.WriteString(output, "\r\n")
		return value.value, value.err
	case <-ctx.Done():
		_ = term.Restore(descriptor, state)
		restored = true
		interruptSecretTerminal(input)
		<-done
		return "", ctx.Err()
	}
}

func readHiddenLine(input io.Reader, limit int) (string, error) {
	value := make([]byte, 0, min(limit, 256))
	var one [1]byte
	for {
		if _, err := io.ReadFull(input, one[:]); err != nil {
			return "", err
		}
		switch one[0] {
		case 3:
			return "", context.Canceled
		case 4:
			return "", io.EOF
		case '\r', '\n':
			if !utf8.Valid(value) {
				return "", errors.New("unable to read UTF-8 secret input")
			}
			return string(value), nil
		case 8, 127:
			if len(value) > 0 {
				_, size := utf8.DecodeLastRune(value)
				value = value[:len(value)-size]
			}
		default:
			value = append(value, one[0])
			if len(value) > limit {
				return "", fmt.Errorf("secret input exceeds %d UTF-8 bytes", limit)
			}
		}
	}
}
