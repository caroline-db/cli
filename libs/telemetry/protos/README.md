The types in this package are equivalent to the lumberjack protos defined in Universe.
You can find all lumberjack protos for the Databricks CLI in the `proto/logs/frontend/databricks_cli` directory.

`databricks air run` logs one `air_run_event` per submission: requested compute,
which config options were set, the outcome and latency, and how long packaging and
uploading the code snapshot took. It never includes code, paths, names, parameters,
or image references. Set `DATABRICKS_CLI_DISABLE_TELEMETRY=1` to opt out.

The CLI logs an `aitools_hint_event` each time it prints the Claude Code install hint (any
non-aitools command) or the `apps init` skills tip or install prompt. It carries only the kind of
hint (`hint_kind`); the command it was shown on is the entry's `execution_context.command`. The
event uploads at exit only when the command resolved auth.

`databricks aitools install` logs an `aitools_command_event` funnel per run: STARTED, one
STEP_COMPLETED per install step reached, and FINISHED with the outcome and error category. It has
no calling-agent field: the upload's User-Agent carries the agent (`agent/<name>`) and the
`cmd-exec-id` that every event of the run shares. It uploads at exit only when the command resolved
auth.
