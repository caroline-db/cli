package aircmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/databricks/cli/cmd/root"

	"github.com/spf13/cobra"
)

// Spec declares the air command.
var Spec = root.Spec{
	Use:     "air",
	Short:   "*Public Preview* Run and manage AI Runtime training workloads",
	GroupID: "development",
	Build:   buildCommand,
}

func buildCommand(ctx context.Context, cmd *cobra.Command) {
	cmd.Long = `Run and manage AI Runtime training workloads on Databricks serverless GPU compute.

These commands are under active development.`

	runCommand := newRunCommand()
	wrapRunErrorWithDebugTip(runCommand)
	cmd.AddCommand(runCommand)
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newPoolsCommand())
	cmd.AddCommand(newLogsCommand())
	cmd.AddCommand(newCancelCommand())
	cmd.AddCommand(newConvertToDabsCommand())
	cmd.AddCommand(newImagesCommand())
}

func wrapRunErrorWithDebugTip(cmd *cobra.Command) {
	runE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return withDebugErrorTip(cmd, runE(cmd, args))
	}
}

func withDebugErrorTip(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	debugFlag := cmd.Root().PersistentFlags().Lookup("debug")
	if debugFlag != nil && debugFlag.Value.String() == "true" {
		return err
	}

	commandPrefix := cmd.Root().CommandPath()
	command := commandPrefix + " --debug" + strings.TrimPrefix(cmd.CommandPath(), commandPrefix)
	return fmt.Errorf(
		"%w\n\nTip: use the --debug flag to see more details and a trace of this error:\n  %s …",
		err,
		command,
	)
}
