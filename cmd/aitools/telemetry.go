package aitools

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/databricks/cli/cmd/root"
	"github.com/databricks/cli/libs/aitools/agents"
	"github.com/databricks/cli/libs/aitools/installer"
	"github.com/databricks/cli/libs/cmdctx"
	"github.com/databricks/cli/libs/cmdio"
	"github.com/databricks/cli/libs/log"
	"github.com/databricks/cli/libs/telemetry"
	"github.com/databricks/cli/libs/telemetry/protos"
	"github.com/databricks/databricks-sdk-go/config"
	"github.com/spf13/cobra"
)

// telemetryAuthBudget bounds how long a command waits, at the end of its run,
// for the best-effort auth resolution that lets its telemetry upload. The
// resolution runs in the background while the command works, so the command
// waits only for the part of it that outlasts its run, and only until this
// budget after its start; a resolution still unfinished at the later of the
// end of the run and that deadline drops the events. Matches the upload's own
// default timeout. A resolution that does finish is followed by the upload,
// which has its own timeout, so after a short run a slow but reachable
// workspace can add up to about twice this.
//
// While the workspace stays unreachable, every run takes at least this long
// (the budget starts when the command does, so a run that takes longer waits
// nothing at the end), including runs that end early, such as a
// flag-validation error (they still log funnel events): the SDK retries the
// host-metadata request on connection errors, so the fetch outlasts the budget
// and the process exits before libs/hostmetadata writes its negative-cache
// entry, and the next run probes the host again.
var telemetryAuthBudget = 3 * time.Second

// resolveTelemetryAuthFn resolves auth onto cmd's context. A seam for tests.
var resolveTelemetryAuthFn = func(cmd *cobra.Command, args []string) {
	_, _ = root.MustAnyClient(cmd, args)
}

// withTelemetryAuth makes cmd resolve auth best-effort so its telemetry can
// upload at exit: PreRunE starts the resolution in the background and RunE's
// wrapper waits for it within telemetryAuthBudget. Nothing is resolved when
// telemetry is disabled. The command never fails or prompts because of it.
func withTelemetryAuth(cmd *cobra.Command) {
	var pending *pendingAuth
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		pending = startTelemetryAuth(cmd, args)
		return nil
	}
	runE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := runE(cmd, args)
		pending.apply(cmd)
		return err
	}
}

// pendingAuth is a background auth resolution started by startTelemetryAuth.
type pendingAuth struct {
	done     chan struct{}
	deadline time.Time
	cfg      *config.Config
}

// startTelemetryAuth resolves auth on a shadow command that shares cmd's
// flags (for --profile) but not its context, so a resolution still running
// when the budget expires never touches the command's context. The shadow
// logs at Debug only: it runs concurrently with the command's output and
// prompts, and its errors are discarded anyway. Lines logged on a context
// without the shadow's logger still use the default logger: any SDK line on
// its own context.Background() (e.g. host metadata without an
// oidc_endpoint), and the host-metadata lookup of the databricks-cli
// credential strategy's own config (libs/auth ToOAuthArgument), which warns
// when the host is unreachable and the cache is off or unusable. A panic is
// recovered and logged, as the command must not fail because of it.
//
// The resolution authenticates, which for the databricks-cli strategy can
// refresh the OAuth token. A refresh still in flight when the command stops
// waiting (the later of the end of its run and the budget) is abandoned at
// exit: if the server has already rotated the single-use
// refresh token, the new one is never stored, and the next command fails to
// refresh until the user logs in again.
func startTelemetryAuth(cmd *cobra.Command, args []string) *pendingAuth {
	ctx := cmd.Context()
	if telemetry.Disabled(ctx) {
		return nil
	}
	sctx := log.NewContext(ctx, slog.New(debugOnlyHandler{log.GetLogger(ctx).Handler()}))
	shadow := &cobra.Command{Use: cmd.Use}
	shadow.Flags().AddFlagSet(cmd.Flags())
	shadow.SetContext(root.SkipLoadBundle(root.SkipPrompt(sctx)))

	p := &pendingAuth{done: make(chan struct{}), deadline: time.Now().Add(telemetryAuthBudget)}
	resolve := resolveTelemetryAuthFn
	go func() {
		defer close(p.done)
		defer func() {
			if r := recover(); r != nil {
				log.Debugf(sctx, "auth for telemetry panicked: %v", r)
			}
		}()
		resolve(shadow, args)
		if sctx := shadow.Context(); cmdctx.HasConfigUsed(sctx) {
			p.cfg = cmdctx.ConfigUsed(sctx)
		}
	}()
	return p
}

// apply waits for the resolution until the deadline and, if it finished, puts
// the resolved config on cmd's context for the upload. Only the config is
// copied: the upload needs nothing else, and a workspace client on the context
// would change how root decorates the command's own errors.
func (p *pendingAuth) apply(cmd *cobra.Command) {
	if p == nil {
		return
	}
	if !p.wait() {
		log.Debugf(cmd.Context(), "auth for telemetry not resolved within %s; not uploading telemetry", telemetryAuthBudget)
		return
	}
	if p.cfg != nil {
		cmd.SetContext(cmdctx.SetConfigUsed(cmd.Context(), p.cfg))
	}
}

// wait reports whether the resolution finished by the deadline. A resolution
// that finished before the wait starts always counts: once a command outlasts
// the budget, a select on both channels would pick between them at random.
func (p *pendingAuth) wait() bool {
	select {
	case <-p.done:
		return true
	default:
	}
	timer := time.NewTimer(time.Until(p.deadline))
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// debugOnlyHandler logs records above Debug (e.g. hostmetadata's "failed to
// fetch host metadata" Warn) at Debug, so they show only with --debug.
type debugOnlyHandler struct{ slog.Handler }

func debugOnly(level slog.Level) slog.Level {
	return min(level, log.LevelDebug)
}

func (h debugOnlyHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, debugOnly(level))
}

func (h debugOnlyHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Level = debugOnly(r.Level)
	return h.Handler.Handle(ctx, r)
}

func (h debugOnlyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return debugOnlyHandler{h.Handler.WithAttrs(attrs)}
}

func (h debugOnlyHandler) WithGroup(name string) slog.Handler {
	return debugOnlyHandler{h.Handler.WithGroup(name)}
}

// installOpts is the subset of install options telemetry records, kept narrow
// so every field that leaves the machine is visible here.
type installOpts struct {
	Scope        string
	Experimental bool
}

// logInstallEvent buffers an install event; cmd/root uploads it at exit.
// errCategory is the top-level command outcome (Unspecified on success or when
// the failure is reported per agent), and outcomes carries the per-agent results
// so a skipped-with-warning failure is still recorded even when the command
// exits 0.
func logInstallEvent(ctx context.Context, plan []agentPlanItem, opts installOpts, errCategory protos.AitoolsErrorCategory, outcomes []agentOutcome) {
	telemetry.Log(ctx, protos.DatabricksCliLog{
		AitoolsInstallEvent: &protos.AitoolsInstallEvent{
			Agents:        agentsField(plan),
			Scope:         scopeType(opts.Scope),
			Experimental:  opts.Experimental,
			ErrorCategory: errCategory,
			AgentResults:  agentResultsField(outcomes),
		},
	})
}

// agentResultsField returns the per-agent failure/skip categories, one entry per
// non-successful agent, sorted by agent enum for stable output. Successful
// agents produce no entry.
func agentResultsField(outcomes []agentOutcome) []protos.AitoolsAgentResult {
	var out []protos.AitoolsAgentResult
	for _, o := range outcomes {
		// Successful agents produce no entry; only failed/skipped outcomes carry a
		// category. Keying on status (rather than errorCategory == "") avoids a trap
		// where a future Unspecified category on a success would emit a bogus entry.
		if o.status == outcomeInstalled {
			continue
		}
		out = append(out, protos.AitoolsAgentResult{
			Agent:         agentType(o.agent.Name),
			ErrorCategory: o.errorCategory,
		})
	}
	slices.SortFunc(out, func(a, b protos.AitoolsAgentResult) int {
		return cmp.Compare(a.Agent, b.Agent)
	})
	return out
}

// agentsField returns the deduped agent enums from the plan, sorted so the
// same set of agents always produces the same array on the analytics side.
func agentsField(plan []agentPlanItem) []protos.AitoolsAgentType {
	if len(plan) == 0 {
		return nil
	}
	out := make([]protos.AitoolsAgentType, 0, len(plan))
	seen := make(map[protos.AitoolsAgentType]struct{}, len(plan))
	for _, it := range plan {
		if it.agent == nil {
			continue
		}
		t := agentType(it.agent.Name)
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// agentType maps a CLI agent registry name to its telemetry enum. Every agent
// in agents.Registry must have a case here; TestAgentTypeCoversRegistry fails
// if one is missing. The default maps genuinely unknown names to Unspecified
// so an older proto never drops an install.
func agentType(name string) protos.AitoolsAgentType {
	switch name {
	case agents.NameClaudeCode:
		return protos.AitoolsAgentTypeClaudeCode
	case agents.NameCursor:
		return protos.AitoolsAgentTypeCursor
	case agents.NameCodex:
		return protos.AitoolsAgentTypeCodex
	case agents.NameOpenCode:
		return protos.AitoolsAgentTypeOpenCode
	case agents.NameCopilot:
		return protos.AitoolsAgentTypeCopilot
	case agents.NameAntigravity:
		return protos.AitoolsAgentTypeAntigravity
	case agents.NamePi:
		return protos.AitoolsAgentTypePi
	case agents.NameGemini:
		return protos.AitoolsAgentTypeGemini
	case agents.NameGoose:
		return protos.AitoolsAgentTypeGoose
	case agents.NameKiro:
		return protos.AitoolsAgentTypeKiro
	default:
		return protos.AitoolsAgentTypeUnspecified
	}
}

// scopeType maps a resolved install scope to its telemetry enum.
func scopeType(scope string) protos.AitoolsInstallScope {
	switch scope {
	case installer.ScopeGlobal:
		return protos.AitoolsInstallScopeGlobal
	case installer.ScopeProject:
		return protos.AitoolsInstallScopeProject
	default:
		return protos.AitoolsInstallScopeUnspecified
	}
}

// commandFunnel emits the AitoolsCommandEvent funnel of one aitools subcommand
// run: STARTED when created, STEP_COMPLETED per step, and exactly one FINISHED.
// The scope from setScope is attached to every later event; the agents from
// setAgents only to FINISHED.
type commandFunnel struct {
	command     protos.AitoolsCommand
	interactive bool
	installMode protos.AitoolsInstallMode
	scope       protos.AitoolsInstallScope
	agents      []protos.AitoolsAgentType
	finished    bool
}

// startFunnel logs the STARTED event for command and returns the funnel to
// report later steps and the outcome on.
// installMode is set for install only, so it is on the STARTED event too.
func startFunnel(ctx context.Context, command protos.AitoolsCommand, installMode protos.AitoolsInstallMode) *commandFunnel {
	f := &commandFunnel{
		command:     command,
		interactive: cmdio.IsPromptSupported(ctx),
		installMode: installMode,
	}
	f.log(ctx, protos.AitoolsCommandEvent{Stage: protos.AitoolsCommandStageStarted})
	return f
}

// setScope records the resolved scope (installer.ScopeGlobal/ScopeProject).
func (f *commandFunnel) setScope(scope string) { f.scope = scopeType(scope) }

// setAgents records the targeted agents, reported on the FINISHED event.
func (f *commandFunnel) setAgents(targets []*agents.Agent) {
	plan := make([]agentPlanItem, 0, len(targets))
	for _, a := range targets {
		plan = append(plan, agentPlanItem{agent: a})
	}
	f.agents = agentsField(plan)
}

// step logs a STEP_COMPLETED event.
func (f *commandFunnel) step(ctx context.Context, s protos.AitoolsCommandStep) {
	f.log(ctx, protos.AitoolsCommandEvent{
		Stage: protos.AitoolsCommandStageStepCompleted,
		Step:  s,
	})
}

// finish logs the FINISHED event for a run that returned err: SUCCESS for nil,
// NOTHING_TO_DO when update or uninstall found nothing installed (the command
// still exits non-zero), CANCELLED when the user aborted a prompt or picked no
// agents, FAILURE otherwise. No-op once the run has finished, so it is safe to
// defer after an explicit finishWith.
func (f *commandFunnel) finish(ctx context.Context, err error) {
	switch {
	case err == nil:
		f.finishWith(ctx, protos.AitoolsCommandOutcomeSuccess, protos.AitoolsErrorCategoryUnspecified)
	case errors.Is(err, installer.ErrNothingInstalled):
		f.finishWith(ctx, protos.AitoolsCommandOutcomeNothingToDo, protos.AitoolsErrorCategoryUnspecified)
	case errors.Is(err, huh.ErrUserAborted):
		f.finishWith(ctx, protos.AitoolsCommandOutcomeCancelled, protos.AitoolsErrorCategoryUserAborted)
	case errors.Is(err, errNoAgentsSelected):
		f.finishWith(ctx, protos.AitoolsCommandOutcomeCancelled, protos.AitoolsErrorCategoryUnspecified)
	default:
		f.finishWith(ctx, protos.AitoolsCommandOutcomeFailure, classifyInstallError(err))
	}
}

// finishWith logs the FINISHED event with an explicit outcome, for runs whose
// returned error does not tell it: a nil return without success (declined
// confirm, nothing to act on, nothing installed) or a named agent's failure.
func (f *commandFunnel) finishWith(ctx context.Context, outcome protos.AitoolsCommandOutcome, category protos.AitoolsErrorCategory) {
	if f.finished {
		return
	}
	f.finished = true
	// Unspecified means "no error"; omit it rather than send the sentinel.
	if category == protos.AitoolsErrorCategoryUnspecified {
		category = ""
	}
	f.log(ctx, protos.AitoolsCommandEvent{
		Stage:         protos.AitoolsCommandStageFinished,
		Outcome:       outcome,
		ErrorCategory: category,
		Agents:        f.agents,
	})
}

func (f *commandFunnel) log(ctx context.Context, e protos.AitoolsCommandEvent) {
	e.Command = f.command
	e.Interactive = f.interactive
	e.InstallMode = f.installMode
	e.Scope = f.scope
	telemetry.Log(ctx, protos.DatabricksCliLog{AitoolsCommandEvent: &e})
}

// withCommandFunnel wraps a subcommand's RunE so every run logs STARTED and
// FINISHED (outcome from the returned error).
func withCommandFunnel(command protos.AitoolsCommand, runE func(cmd *cobra.Command, args []string) error) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) (err error) {
		f := startFunnel(cmd.Context(), command, "")
		defer func() { f.finish(cmd.Context(), err) }()
		cmd.SetContext(context.WithValue(cmd.Context(), funnelKey{}, f))
		return runE(cmd, args)
	}
}

type funnelKey struct{}

// funnelCancelled records a run the user cancelled at a prompt while the
// command still returns nil (e.g. answering "no" to uninstall's confirm), so
// withCommandFunnel does not report it as SUCCESS. No-op outside a funnel.
func funnelCancelled(ctx context.Context) {
	if f, ok := ctx.Value(funnelKey{}).(*commandFunnel); ok {
		f.finishWith(ctx, protos.AitoolsCommandOutcomeCancelled, protos.AitoolsErrorCategoryUnspecified)
	}
}

// funnelAllAttemptsFailed records a run that returns nil (plugin failures and
// skill removal failures are warnings) but did nothing, because every plugin or
// skill operation it attempted failed or was skipped, as FAILURE with the first
// cause's category. No-op outside a funnel.
func funnelAllAttemptsFailed(ctx context.Context, firstErr error) {
	if f, ok := ctx.Value(funnelKey{}).(*commandFunnel); ok {
		f.finishWith(ctx, protos.AitoolsCommandOutcomeFailure, classifyInstallError(firstErr))
	}
}

// installModeType maps the install delivery flags to the telemetry enum.
func installModeType(skillsOnly bool, path string) protos.AitoolsInstallMode {
	switch {
	case path != "":
		return protos.AitoolsInstallModePath
	case skillsOnly:
		return protos.AitoolsInstallModeSkillsOnly
	default:
		return protos.AitoolsInstallModeDefault
	}
}

// deliverySteps returns the delivery steps executePlan got through, derived
// from its outcomes: SKILLS_INSTALLED when the skills group installed, and
// PLUGINS_INSTALLED when at least one plugin installed. Events are
// uploaded together at exit, so logging them after executePlan returns yields
// the same payload as logging them inside it.
func deliverySteps(outcomes []agentOutcome) []protos.AitoolsCommandStep {
	var skills, plugins bool
	for _, o := range outcomes {
		switch o.delivery {
		case deliverySkills:
			skills = skills || o.status == outcomeInstalled
		case deliveryPlugin:
			plugins = plugins || o.status == outcomeInstalled
		case deliverySkip:
		}
	}
	var steps []protos.AitoolsCommandStep
	if skills {
		steps = append(steps, protos.AitoolsCommandStepSkillsInstalled)
	}
	if plugins {
		steps = append(steps, protos.AitoolsCommandStepPluginsInstalled)
	}
	return steps
}

// instrumentCommand makes an aitools subcommand log its STARTED and FINISHED
// funnel events, and resolves auth best-effort so they can upload at exit.
func instrumentCommand(cmd *cobra.Command, command protos.AitoolsCommand) *cobra.Command {
	cmd.RunE = withCommandFunnel(command, cmd.RunE)
	withTelemetryAuth(cmd)
	return cmd
}

// nothingInstalledCategory reports whether a run that returned no error still
// installed nothing for any agent (e.g. the only detected agent's plugin
// install failed, which is a warning, not an error), and if so the category of
// the first agent that failed or was skipped. The funnel records such a run as
// FAILURE rather than SUCCESS. An agent that kept its plugin is set up, so it
// is not a failure by itself (see onlyKeptPlugins), but another agent's failed
// install still is, and its category is the one reported.
func nothingInstalledCategory(outcomes []agentOutcome) (protos.AitoolsErrorCategory, bool) {
	if len(outcomes) == 0 {
		return "", false
	}
	kept := false
	var failed []agentOutcome
	for _, o := range outcomes {
		switch o.status {
		case outcomeInstalled:
			return "", false
		case outcomeKept:
			kept = true
		case outcomeFailed:
			failed = append(failed, o)
		case outcomeSkipped:
		}
	}
	if len(failed) > 0 {
		return firstFailureCategory(failed), true
	}
	if kept {
		return "", false
	}
	return firstFailureCategory(outcomes), true
}

// onlyKeptPlugins reports whether a run installed nothing, failed for no agent,
// and left at least one agent with the plugin it already had. The funnel
// records such a run as NOTHING_TO_DO rather than SUCCESS.
func onlyKeptPlugins(outcomes []agentOutcome) bool {
	kept := false
	for _, o := range outcomes {
		if o.status == outcomeInstalled || o.status == outcomeFailed {
			return false
		}
		kept = kept || o.status == outcomeKept
	}
	return kept
}

// firstFailureCategory returns the category of the first agent that failed or
// was skipped, or UNCATEGORIZED when none carries one.
func firstFailureCategory(outcomes []agentOutcome) protos.AitoolsErrorCategory {
	for _, o := range outcomes {
		if o.status != outcomeInstalled && o.errorCategory != "" && o.errorCategory != protos.AitoolsErrorCategoryUnspecified {
			return o.errorCategory
		}
	}
	return protos.AitoolsErrorCategoryUncategorized
}
