package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Retain unrecognized fields, including fields in grants and enrollments, so a
// partial migration cannot erase data written by a newer command group.
type configuration map[string]json.RawMessage

func (config configuration) stringValue(name string) *string {
	var value *string
	_ = json.Unmarshal(config[name], &value)
	return value
}

func configPath() (string, error) {
	if root := os.Getenv("CO_CONFIG_DIR"); root != "" {
		return filepath.Join(root, "config.json"), nil
	}
	if root, present := os.LookupEnv("XDG_CONFIG_HOME"); present {
		return filepath.Join(root, "co", "config.json"), nil
	}
	return defaultConfigPath()
}

func effectiveAPI(config configuration) string {
	api, present := os.LookupEnv("CO_API_URL")
	if !present {
		api = "https://api.co.codes"
		if stored := config.stringValue("api_url"); stored != nil {
			api = *stored
		}
	}
	return strings.TrimRight(api, "/")
}

func loadConfig() (configuration, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := readPrivateConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return nil, fmt.Errorf("unable to read %s: %s", path, processError(err))
	}
	config, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("invalid config %s: invalid configuration JSON", path)
	}
	return config, nil
}

func parseConfig(data []byte) (configuration, error) {
	if !utf8.Valid(data) || !validConfigUnicode(data) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := uniqueJSON(decoder); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	config, err := configObject(data, []string{"api_url", "session_token", "upstream_name", "jj", "agents", "pending_agent_submissions", "pending_agent_requests", "agent_grants", "machines"})
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"api_url", "session_token", "upstream_name"} {
		if err := configField(config, name, "string", "null", true); err != nil {
			return nil, err
		}
	}
	if err := configField(config, "jj", "bool", "false", false); err != nil {
		return nil, err
	}
	schemas := []struct {
		name       string
		required   map[string]string
		optional   map[string]string
		defaultOps bool
	}{
		{"agents", map[string]string{"id": "string", "name": "string"}, nil, false},
		{"pending_agent_submissions", map[string]string{"agent_id": "string", "owner": "string", "repo": "string", "operations": "strings", "ttl_seconds": "uint", "request_capability": "string", "idempotency_key": "string"}, map[string]string{"reason": "string"}, false},
		{"pending_agent_requests", map[string]string{"id": "string", "agent_id": "string", "owner": "string", "repo": "string", "poll_token": "string", "requested_expires_unix": "uint"}, map[string]string{"approval_expires_unix": "uint"}, true},
		{"agent_grants", map[string]string{"agent_id": "string", "owner": "string", "repo": "string", "grant_id": "string", "lineage_token": "string", "expires_unix": "uint"}, nil, true},
		{"machines", map[string]string{"id": "string", "scope": "string", "api_url": "string", "credential": "string"}, nil, false},
	}
	orders := map[string][]string{
		"agents":                    {"id", "name"},
		"pending_agent_submissions": {"agent_id", "owner", "repo", "operations", "ttl_seconds", "reason", "request_capability", "idempotency_key"},
		"pending_agent_requests":    {"id", "agent_id", "owner", "repo", "poll_token", "operations", "requested_expires_unix", "approval_expires_unix"},
		"agent_grants":              {"agent_id", "owner", "repo", "grant_id", "lineage_token", "operations", "expires_unix"},
		"machines":                  {"id", "scope", "api_url", "credential"},
	}
	for _, schema := range schemas {
		if _, exists := config[schema.name]; !exists {
			config[schema.name] = json.RawMessage("[]")
		}
		var rawRecords []json.RawMessage
		if err := json.Unmarshal(config[schema.name], &rawRecords); err != nil || rawRecords == nil {
			return nil, errors.New("expected records")
		}
		records := make([]configuration, 0, len(rawRecords))
		for _, raw := range rawRecords {
			record, err := configObject(raw, orders[schema.name])
			if err != nil {
				return nil, err
			}
			for name, kind := range schema.required {
				if err := configField(record, name, kind, "", false); err != nil {
					return nil, err
				}
			}
			for name, kind := range schema.optional {
				if err := configField(record, name, kind, "null", true); err != nil {
					return nil, err
				}
			}
			if schema.defaultOps {
				if err := configField(record, "operations", "strings", "[]", false); err != nil {
					return nil, err
				}
			}
			records = append(records, record)
		}
		config[schema.name], _ = json.Marshal(records)
	}
	return config, nil
}

// serde also accepts positional JSON arrays for structs. Read those legacy
// shapes in field order, but publish normal named-field objects.
func configObject(data []byte, order []string) (configuration, error) {
	var object configuration
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		if len(values) > len(order) {
			return nil, errors.New("too many record fields")
		}
		object = configuration{}
		for index, value := range values {
			object[order[index]] = value
		}
		return object, nil
	}
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("expected record")
	}
	return object, nil
}

func validConfigUnicode(data []byte) bool {
	quoted := false
	for index := 0; index < len(data); index++ {
		if data[index] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || data[index] != '\\' {
			continue
		}
		index++
		if index >= len(data) {
			return false
		}
		if data[index] != 'u' {
			continue
		}
		if index+4 >= len(data) {
			return false
		}
		value, err := strconv.ParseUint(string(data[index+1:index+5]), 16, 16)
		if err != nil {
			return false
		}
		index += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if index+6 >= len(data) || data[index+1] != '\\' || data[index+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[index+3:index+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			index += 6
		}
	}
	return true
}

func configField(config configuration, name, kind, fallback string, nullable bool) error {
	raw, exists := config[name]
	if !exists && fallback != "" {
		raw = json.RawMessage(fallback)
		config[name] = raw
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if nullable {
			return nil
		}
		return errors.New("null field")
	}
	var err error
	switch kind {
	case "string":
		var value string
		err = json.Unmarshal(raw, &value)
	case "bool":
		var value bool
		err = json.Unmarshal(raw, &value)
	case "strings":
		var items []json.RawMessage
		err = json.Unmarshal(raw, &items)
		for _, item := range items {
			var value string
			if bytes.Equal(bytes.TrimSpace(item), []byte("null")) {
				return errors.New("null string")
			}
			if err := json.Unmarshal(item, &value); err != nil {
				return err
			}
		}
	case "uint":
		_, err = strconv.ParseUint(string(raw), 10, 64)
	}
	return err
}

// Reject duplicate keys instead of accepting an ambiguous credential record.
func uniqueJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); ok {
		keys := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("duplicate JSON key")
				}
				keys[name] = true
			}
			if err := uniqueJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
	}
	return err
}

func mutateConfig(update func(configuration) error) (result error) {
	path, err := configPath()
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := secureConfigDirectory(parent); err != nil {
		return fmt.Errorf("unable to secure %s: %s", parent, processError(err))
	}
	lockPath := path + ".lock"
	lock, err := openConfigLock(lockPath)
	if err != nil {
		return fmt.Errorf("unable to open %s: %s", lockPath, processError(err))
	}
	defer lock.Close()
	if err := lockConfig(lock); err != nil {
		return fmt.Errorf("unable to lock %s: %s", lockPath, processError(err))
	}
	defer func() {
		if err := unlockConfig(lock); err != nil {
			result = fmt.Errorf("unable to unlock %s: %s", lockPath, processError(err))
		}
	}()
	config, err := loadConfig()
	if err != nil {
		return err
	}
	if err := update(config); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if _, err := parseConfig(data); err != nil {
		return errors.New("invalid configuration update")
	}
	temporary, err := createConfigTemp(parent)
	if err != nil {
		return fmt.Errorf("unable to write private config: %s", processError(err))
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("unable to write %s: %s", name, processError(err))
	}
	if err := replaceConfig(name, path); err != nil {
		return fmt.Errorf("unable to replace %s: %s", path, processError(err))
	}
	return nil
}
