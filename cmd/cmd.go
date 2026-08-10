package cmd

import (
	log "github.com/sirupsen/logrus"

	"github.com/spf13/cobra"
)

var command = &cobra.Command{
	Use:           "v2node",
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Run executes the CLI and returns a process-compatible exit code. Keeping the
// decision here makes command failures visible to service managers while a
// clean server shutdown still exits successfully.
func Run() int {
	err := command.Execute()
	if err != nil {
		log.WithField("err", err).Error("Execute command failed")
		return 1
	}
	return 0
}
