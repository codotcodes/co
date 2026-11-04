package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"
)

func apiRecord(response *http.Response, fields []string, prefix string) (configuration, error) {
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
