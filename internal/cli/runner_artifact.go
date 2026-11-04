package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const runnerArtifactLimit = 32 * 1024 * 1024
const artifactCollectionError = "Unable to collect artifact: use a nonempty directory of regular files within the artifact limits."

type runnerArtifactEntry struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

func runnerDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func collectRunnerArtifact(checkout string, output runnerArtifact) ([]byte, error) {
	invalid := errors.New(artifactCollectionError)
	if !validRunnerArtifact(output) {
		return nil, invalid
	}
	directory := checkout
	for _, part := range strings.Split(output.Path, "/") {
		directory = filepath.Join(directory, part)
		stat, err := os.Lstat(directory)
		if err != nil || !stat.IsDir() {
			return nil, invalid
		}
	}
	type content struct {
		path string
		data []byte
	}
	var files []content
	total, visited := 0, 0
	var walk func(string, string) error
	walk = func(directory, prefix string) error {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return invalid
		}
		for _, entry := range entries {
			visited++
			if visited > 8192 {
				return invalid
			}
			path := entry.Name()
			if prefix != "" {
				path = prefix + "/" + path
			}
			if !validArtifactPath(path) {
				return invalid
			}
			full := filepath.Join(directory, entry.Name())
			stat, err := os.Lstat(full)
			if err != nil {
				return invalid
			}
			if stat.IsDir() {
				if err := walk(full, path); err != nil {
					return err
				}
			} else if stat.Mode().IsRegular() {
				if len(files) >= 4096 {
					return invalid
				}
				file, err := openRunnerRegular(full, int64(runnerArtifactLimit-total), stat)
				if err != nil {
					return invalid
				}
				opened, err := file.Stat()
				if err != nil {
					file.Close()
					return invalid
				}
				data, err := io.ReadAll(io.LimitReader(file, int64(runnerArtifactLimit-total+1)))
				file.Close()
				if err != nil || len(data) > runnerArtifactLimit-total || int64(len(data)) != opened.Size() {
					return invalid
				}
				total += len(data)
				files = append(files, content{path, data})
			} else {
				return invalid
			}
		}
		return nil
	}
	if err := walk(directory, ""); err != nil || len(files) == 0 {
		return nil, invalid
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	manifest := make([]runnerArtifactEntry, 0, len(files))
	for _, file := range files {
		manifest = append(manifest, runnerArtifactEntry{Path: file.path, Size: len(file.data), SHA256: runnerDigest(file.data)})
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > 1024*1024 || 10+len(encoded)+total > runnerArtifactLimit {
		return nil, invalid
	}
	result := make([]byte, 10, 10+len(encoded)+total)
	copy(result, "COAR1\x00")
	binary.BigEndian.PutUint32(result[6:10], uint32(len(encoded)))
	result = append(result, encoded...)
	for _, file := range files {
		result = append(result, file.data...)
	}
	return result, nil
}
