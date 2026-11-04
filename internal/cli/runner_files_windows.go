package cli

import (
	"errors"
	"os"
)

func openRunnerRegular(string, int64, os.FileInfo) (*os.File, error) {
	return nil, errors.New("runner artifact and action output collection requires Unix")
}

func openRunnerLock(string) (*os.File, error) {
	return nil, errors.New("runner cache requires Unix")
}

func openRunnerEnrollmentLock(path string) (*os.File, error) { return openConfigLock(path) }
