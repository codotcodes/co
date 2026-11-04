//go:build !windows

package cli

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func defaultConfigPath() (string, error) {
	home, present := os.LookupEnv("HOME")
	if !present {
		return "", errors.New("HOME is unset; set CO_CONFIG_DIR or XDG_CONFIG_HOME for co")
	}
	return filepath.Join(home, ".config", "co", "config.json"), nil
}

func secureConfigDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func readPrivateConfig(path string) ([]byte, error) { return os.ReadFile(path) }
func openConfigLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
func lockConfig(file *os.File) error   { return unix.Flock(int(file.Fd()), unix.LOCK_EX) }
func unlockConfig(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
func createConfigTemp(parent string) (*os.File, error) {
	return os.CreateTemp(parent, "config.json.*.tmp")
}
func replaceConfig(source, destination string) error { return os.Rename(source, destination) }
