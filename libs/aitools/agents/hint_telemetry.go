package agents

import (
	"context"

	"github.com/databricks/cli/libs/telemetry"
	"github.com/databricks/cli/libs/telemetry/protos"
)

// LogHintShown records that a recommendation to install Databricks AI tools of
// the given kind was shown. Call it once per hint shown; for a prompt, call it
// before asking so a declined prompt still counts. The upload's
// ExecutionContext.Command names the command the hint was shown on.
//
// The event is buffered and uploaded by cmd/root at exit, which needs resolved
// auth: a hint shown on a command that never resolves auth (e.g. `databricks
// version`) is not uploaded.
func LogHintShown(ctx context.Context, kind protos.AitoolsHintKind) {
	telemetry.Log(ctx, protos.DatabricksCliLog{
		AitoolsHintEvent: &protos.AitoolsHintEvent{HintKind: kind},
	})
}
