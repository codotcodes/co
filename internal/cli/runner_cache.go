package cli

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const runnerCacheHostLimit = 1024 * 1024 * 1024

var invalidRunnerCache = errors.New("cache input is invalid or exceeds its limits")

type runnerCacheStore struct {
	root, namespace string
}

type runnerCachePublication struct {
	store runnerCacheStore
	id    string
	data  []byte
}

type runnerCacheRequest struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

type runnerCacheEntry struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

func runnerCacheDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	stat, err := os.Lstat(path)
	if err != nil || !stat.IsDir() {
		return invalidRunnerCache
	}
	return os.Chmod(path, 0700)
}

func readRunnerCacheFile(path string, limit int) ([]byte, error) {
	file, err := openRunnerRegular(path, int64(limit), nil)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit+1)))
	if err != nil || len(data) > limit {
		return nil, invalidRunnerCache
	}
	return data, nil
}

func runnerCachePath(value string) bool {
	if len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
		for _, character := range part {
			if unicode.IsControl(character) || character == '\\' {
				return false
			}
		}
	}
	return true
}

func runnerDirectoryEntries(path string, count int) ([]os.DirEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(count)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return entries, nil
}

func collectRunnerCacheBundle(root string) ([]byte, error) {
	stat, err := os.Lstat(root)
	if err != nil || !stat.IsDir() {
		return nil, invalidRunnerCache
	}
	entries := []runnerCacheEntry{}
	var data []byte
	visited := 0
	var walk func(string, string) error
	walk = func(root, prefix string) error {
		files, err := runnerDirectoryEntries(root, -1)
		if err != nil {
			return err
		}
		for _, file := range files {
			visited++
			if visited > 8192 {
				return invalidRunnerCache
			}
			path := file.Name()
			if prefix != "" {
				path = prefix + "/" + path
			}
			if !runnerCachePath(path) {
				return invalidRunnerCache
			}
			full := filepath.Join(root, file.Name())
			stat, err := os.Lstat(full)
			if err != nil {
				return err
			}
			if stat.IsDir() {
				if err := walk(full, path); err != nil {
					return err
				}
			} else if stat.Mode().IsRegular() && len(entries) < 4096 {
				bytes, err := readRunnerCacheFile(full, runnerArtifactLimit-len(data))
				if err != nil {
					return err
				}
				mode := uint32(0644)
				if stat.Mode().Perm()&0111 != 0 {
					mode = 0755
				}
				entries = append(entries, runnerCacheEntry{Path: path, Size: len(bytes), SHA256: runnerDigest(bytes), Mode: mode})
				data = append(data, bytes...)
			} else {
				return invalidRunnerCache
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	manifest := canonicalRunnerJSON(entries)
	if len(entries) == 0 || len(manifest) > 1024*1024 || len(data)+len(manifest)+10 > runnerArtifactLimit {
		return nil, invalidRunnerCache
	}
	result := make([]byte, 10, len(data)+len(manifest)+10)
	copy(result, "COAR1\x00")
	binary.BigEndian.PutUint32(result[6:10], uint32(len(manifest)))
	result = append(result, manifest...)
	return append(result, data...), nil
}

func openRunnerCache(root, checkoutURL, image string) (runnerCacheStore, error) {
	store := runnerCacheStore{root: root}
	if err := runnerCacheDirectory(root); err != nil {
		return store, err
	}
	marker := filepath.Join(root, ".co-cache-v1")
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		entries, err := runnerDirectoryEntries(root, 1)
		if err != nil || len(entries) != 0 {
			return store, invalidRunnerCache
		}
		file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, err = file.WriteString("co-cache-v1")
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		} else if errors.Is(err, os.ErrExist) {
			err = nil
		}
		if err != nil {
			return store, err
		}
	}
	if bytes, err := readRunnerCacheFile(marker, 32); err != nil || string(bytes) != "co-cache-v1" {
		return store, invalidRunnerCache
	}
	store.namespace = filepath.Join(root, runnerDigest([]byte(checkoutURL+"\x00"+image)))
	if _, err := os.Stat(store.namespace); errors.Is(err, os.ErrNotExist) {
		entries, err := runnerDirectoryEntries(root, 258)
		if err != nil || len(entries) > 256 {
			return store, invalidRunnerCache
		}
	}
	if err := runnerCacheDirectory(store.namespace); err != nil {
		return store, err
	}
	return store, nil
}

func (store runnerCacheStore) collect(checkout, runtime string) ([]runnerCachePublication, error) {
	data, err := readRunnerCacheFile(filepath.Join(runtime, "cache-requests.json"), 65_536)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var requests []json.RawMessage
	if err := json.Unmarshal(data, &requests); err != nil || requests == nil || len(requests) > 16 {
		return nil, invalidRunnerCache
	}
	var result []runnerCachePublication
	total := 0
	for _, raw := range requests {
		record, err := apiObject(raw, []string{"key", "path"}, "invalid cache request")
		if err != nil {
			return nil, invalidRunnerCache
		}
		for _, key := range []string{"key", "path"} {
			if err := configField(record, key, "string", "", false); err != nil {
				return nil, invalidRunnerCache
			}
		}
		request := runnerCacheRequest{Key: *record.stringValue("key"), Path: *record.stringValue("path")}
		if request.Key == "" || len(request.Key) > 512 || strings.ContainsRune(request.Key, 0) {
			return nil, invalidRunnerCache
		}
		root, relative := checkout, ""
		if strings.HasPrefix(request.Path, "/workspace/") {
			relative = strings.TrimPrefix(request.Path, "/workspace/")
		} else if strings.HasPrefix(request.Path, "/run/co-job/home/") {
			root, relative = runtime, "home/"+strings.TrimPrefix(request.Path, "/run/co-job/home/")
		}
		if !runnerCachePath(relative) {
			return nil, invalidRunnerCache
		}
		target := root
		for _, part := range strings.Split(relative, "/") {
			target = filepath.Join(target, part)
			stat, err := os.Lstat(target)
			if err != nil || !stat.IsDir() {
				return nil, invalidRunnerCache
			}
		}
		id := runnerDigest([]byte(request.Path + "\x00" + request.Key))
		if _, err := os.Stat(filepath.Join(store.namespace, id+".cache")); err == nil {
			continue
		}
		bundle, err := collectRunnerCacheBundle(target)
		if err != nil {
			return nil, err
		}
		total += len(bundle)
		if total > runnerArtifactLimit {
			return nil, invalidRunnerCache
		}
		metadata := canonicalRunnerJSON(map[string]any{"key": request.Key, "path": request.Path, "digest": runnerDigest(bundle)})
		bytes := make([]byte, 10, 10+len(metadata)+len(bundle))
		copy(bytes, "COCA1\x00")
		binary.BigEndian.PutUint32(bytes[6:10], uint32(len(metadata)))
		bytes = append(bytes, metadata...)
		bytes = append(bytes, bundle...)
		result = append(result, runnerCachePublication{store: store, id: id, data: bytes})
	}
	return result, nil
}

func (publication runnerCachePublication) publish() error {
	lock, err := openRunnerLock(filepath.Join(publication.store.root, ".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	locked, err := tryConfigLock(lock)
	if err != nil || !locked {
		return invalidRunnerCache
	}
	defer unlockConfig(lock)
	type entry struct {
		modified time.Time
		path     string
		size     int64
	}
	var entries []entry
	total := int64(0)
	namespaces, err := runnerDirectoryEntries(publication.store.root, 258)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		if len(namespace.Name()) != 64 || !namespace.IsDir() {
			continue
		}
		path := filepath.Join(publication.store.root, namespace.Name())
		files, err := runnerDirectoryEntries(path, 34)
		if err != nil {
			return err
		}
		for _, file := range files {
			if !file.Type().IsRegular() || filepath.Ext(file.Name()) != ".cache" {
				continue
			}
			stat, err := file.Info()
			if err != nil || stat.Size() > runnerCacheHostLimit {
				return invalidRunnerCache
			}
			total += stat.Size()
			entries = append(entries, entry{modified: stat.ModTime(), path: filepath.Join(path, file.Name()), size: stat.Size()})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].modified.Before(entries[j].modified) })
	namespaceCount := 0
	for _, entry := range entries {
		if filepath.Dir(entry.path) == publication.store.namespace {
			namespaceCount++
		}
	}
	for _, entry := range entries {
		withinHostBudget := total+int64(len(publication.data)) <= runnerCacheHostLimit
		if withinHostBudget && namespaceCount < 32 {
			break
		}
		if withinHostBudget && filepath.Dir(entry.path) != publication.store.namespace {
			continue
		}
		if err := os.Remove(entry.path); err != nil {
			return err
		}
		total -= entry.size
		if filepath.Dir(entry.path) == publication.store.namespace {
			namespaceCount--
		}
	}
	file, err := os.CreateTemp(publication.store.namespace, ".publication-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(publication.data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(file.Name(), filepath.Join(publication.store.namespace, publication.id+".cache")); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
