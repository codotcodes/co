package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

func encodedRunnerSecret(secret string) string {
	var result strings.Builder
	for _, character := range []byte(secret) {
		if asciiAlphanumeric(character) || strings.ContainsRune("-_.!~*'()", rune(character)) {
			result.WriteByte(character)
		} else {
			fmt.Fprintf(&result, "%%%02X", character)
		}
	}
	return result.String()
}

func runnerOutputSize(outputs map[string]string) int {
	total := 0
	for key, value := range outputs {
		total += len(key) + len(value)
	}
	return total
}

func collectRunnerActionOutputs(directory string, declared, secrets []string) map[string]string {
	if !validActionOutputKeys(declared) {
		return nil
	}
	file, err := openRunnerRegular(filepath.Join(directory, "outputs.json"), 65_536, nil)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65_537))
	if err != nil || len(data) > 65_536 {
		return nil
	}
	record, err := apiObject(data, nil, "invalid action outputs")
	if err != nil {
		return nil
	}
	var outputs map[string]string
	if err := json.Unmarshal(data, &outputs); err != nil || outputs == nil || len(outputs) > 32 || runnerOutputSize(outputs) > 16_384 {
		return nil
	}
	allowed := make(map[string]bool, len(declared))
	for _, key := range declared {
		allowed[key] = true
	}
	for key, value := range outputs {
		if !allowed[key] || len(value) > 4096 || strings.ContainsRune(value, 0) || string(record[key]) == "null" {
			return nil
		}
	}
	for key, value := range outputs {
		for _, secret := range secrets {
			if secret != "" && (strings.Contains(value, secret) || strings.Contains(strings.ToLower(value), strings.ToLower(encodedRunnerSecret(secret)))) {
				delete(outputs, key)
				break
			}
		}
	}
	if len(outputs) == 0 {
		return nil
	}
	return outputs
}
