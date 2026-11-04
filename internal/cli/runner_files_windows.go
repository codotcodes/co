package cli

import (
	"errors"
	"os"
)

func openRunnerRegular(string, int64, os.FileInfo) (*os.File, error) {
	return nil, errors.New("runner artifact and action output collection requires Unix")
}
