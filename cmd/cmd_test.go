package cmd

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestServerHandlePropagatesRunErrorContract(t *testing.T) {
	// Cobra must receive server failures through RunE so Execute returns a
	// non-nil error and systemd's Restart=on-failure can restart the process.
	if serverCommand.RunE == nil {
		t.Fatal("server command must propagate failures through RunE")
	}
	if serverCommand.Run != nil {
		t.Fatal("server command must not swallow failures through Run")
	}
}

func TestCommandSilencesCobraErrorDuplication(t *testing.T) {
	if !command.SilenceErrors || !command.SilenceUsage {
		t.Fatal("top-level command should let Run log an execution failure once")
	}
}

func TestCobraRunEReturnsFailure(t *testing.T) {
	want := errors.New("startup failed")
	c := &cobra.Command{RunE: func(*cobra.Command, []string) error { return want }}
	if got := c.Execute(); !errors.Is(got, want) {
		t.Fatalf("Execute error = %v, want %v", got, want)
	}
}
