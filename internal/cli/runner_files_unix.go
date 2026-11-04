//go:build !windows

package cli

import (
	"errors"
	"os"
	"syscall"
)

func openRunnerRegular(path string, limit int64, expected os.FileInfo) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > limit || stat.Sys().(*syscall.Stat_t).Nlink != 1 || expected != nil && !os.SameFile(stat, expected) {
		file.Close()
		return nil, errors.New("invalid runner file")
	}
	return file, nil
}
