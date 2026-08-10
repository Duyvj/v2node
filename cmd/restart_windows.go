//go:build windows

package cmd

import "errors"

func restartProcess() error {
	return errors.New("in-process restart is unsupported on Windows")
}
