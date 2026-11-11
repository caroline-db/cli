package aitools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/databricks/cli/cmd/root"
	"github.com/databricks/cli/libs/aitools/agents"
	"github.com/databricks/cli/libs/aitools/installer"
	"github.com/databricks/cli/libs/cmdio"
	"github.com/databricks/cli/libs/flags"
	"github.com/databricks/cli/libs/log"
	"github.com/databricks/cli/libs/telemetry/protos"
	"github.com/databricks/databricks-sdk-go/useragent"
	"github.com/spf13/cobra"
)

// Package-level seams for testability. Tests override these via helpers in
// install_test.go.
var (
	promptAgentSelection     = defaultPromptAgentSelection
	installSkillsForAgentsFn = installer.InstallSkillsForAgents
	installPluginForAgentFn  = installer.InstallPluginForAgent
	recordPluginInstallsFn   = installer.RecordPluginInstalls
	cleanupLegacyFn          = installer.RemoveLegacyRawSkills
	callerIsAgent            = func() bool { return useragent.AgentProvider() != "" }
	pluginAlreadyInstalledFn = pluginAlreadyInstalled
)

// errNoAgentsSelected is returned when the picker is confirmed with nothing
// checked. The funnel reports it as CANCELLED: the user chose to act on nothing.
var errNoAgentsSelected = errors.New("at least one agent must be selected")

// installPromptsSupported reports whether install may show its prompts. Agents
// often run commands in a pseudo-terminal (Cursor, Copilot in the VS Code
// terminal) where stdin looks like a TTY but nobody can answer a TUI prompt, so
// an agent caller always gets the documented non-interactive defaults.
func installPromptsSupported(ctx context.Context) bool {
	return cmdio.IsPromptSupported(ctx) && !callerIsAgent()
}

// delivery is how the databricks tools are delivered to one agent.
type delivery int

const (
	// deliveryPlugin installs the databricks plugin through the agent's own CLI.
	deliveryPlugin delivery = iota
	// deliverySkills copies raw skill files (agents with no headless plugin
	// install: OpenCode, Antigravity, Cursor; or any agent under --skills-only).
	deliverySkills
	// deliverySkip does nothing for the agent and explains why.
	deliverySkip
)

// String returns the delivery name used in `list --output json`, so the install
// plan and the list output name the same thing the same way.
func (d delivery) String() string {
	switch d {
	case deliveryPlugin:
		return "plugin"
	case deliverySkills:
		return "skills"
	case deliverySkip:
		return "skip"
	default:
		return "unknown"
	}
}

// agentPlanItem is the resolved plan for one agent: what we'll do and why.
type agentPlanItem struct {
	agent             *agents.Agent
	delivery          delivery
	cliScope          string                      // installer.ScopeGlobal or installer.ScopeProject
	scope             string                      // agent-native plugin scope (deliveryPlugin only)
	reason            string                      // why the agent is skipped (deliverySkip only)
	skipErrorCategory protos.AitoolsErrorCategory // error category for the skip (deliverySkip only)
	explicit          bool                        // named via --agents (blocking it is an error)
}

// agentChoice is one row in the interactive agent picker.
type agentChoice struct {
	agent     *agents.Agent
	label     string
	preselect bool
}

func NewInstallCmd() *cobra.Command {
	var skillsFlag, agentsFlag, scopeFlag, pathFlag string
	var includeExperimental, skillsOnly bool
	var projectFlag, globalFlag bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Databricks skills and plugins for coding agents",
		Long: `Install Databricks skills and plugins for detected coding agents.

By default this installs the databricks plugin through each agent's own CLI
(Claude Code, Codex, GitHub Copilot). Agents without a headless plugin install
(` + strings.Join(agents.SkillsOnlyNames(), ", ") + `) get raw skill files.

Escape hatches:
  --skills-only          Force raw skill files for every agent (no plugin).
  --skills name1,name2   Install only the named skills (with --skills-only/--path).
  --path <dir>           Write resolved skill files to a directory (no agents, no state).

Agent selection:
  --agents <name>[,...]  Act only on the named agents (works for undetected ones).
  (unset, interactive)   A picker over all known agents, detected ones pre-checked.
  (unset, non-interactive) Act on every detected agent.

Non-interactive runs (no terminal, or called by a coding agent) never prompt:
scope defaults to global and agents to every detected one.

Output:
  --output json          Emit a structured result instead of text. Requires --scope
                         and --agents so the command runs without interactive prompts.

Supported agents: ` + strings.Join(agents.SupportedNames(), ", "),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx := cmd.Context()
			jsonMode := installOutputIsJSON(cmd)
			if jsonMode {
				// JSON mode emits only the structured result on stdout; silence all
				// human-readable progress (via cmdio.LogProgress) so nothing but the
				// JSON document is written.
				ctx = cmdio.WithQuiet(ctx)
			}

			funnel := startFunnel(ctx, protos.AitoolsCommandInstall, installModeType(skillsOnly, pathFlag))
			defer func() { funnel.finish(ctx, err) }()

			if skillsOnly && pathFlag != "" {
				return withCategory(errors.New("cannot use --skills-only with --path; --path always writes raw skill files"), protos.AitoolsErrorCategoryInvalidFlags)
			}

			// --path is a plain file dump with no agents or install state, so there
			// is no per-agent result to report. Reject --output json here rather than
			// letting the dump run and silently emit no JSON.
			if jsonMode && pathFlag != "" {
				return withCategory(errors.New("cannot use --output json with --path; --path writes raw skill files and produces no JSON result"), protos.AitoolsErrorCategoryInvalidFlags)
			}

			opts := installer.InstallOptions{
				IncludeExperimental: includeExperimental,
				SpecificSkills:      splitAndTrim(skillsFlag),
			}

			// --skills cherry-picks individual skill files, which only applies to
			// raw-skills delivery. The plugin is installed in full, so reject
			// --skills unless raw skills were requested via --skills-only or --path.
			if len(opts.SpecificSkills) > 0 && !skillsOnly && pathFlag == "" {
				return withCategory(errors.New("--skills requires --skills-only or --path; the databricks plugin is installed in full"), protos.AitoolsErrorCategoryInvalidFlags)
			}

			src := &installer.GitHubManifestSource{}

			// --path is a dumb dump: no agents, no scope, no state.
			if pathFlag != "" {
				funnel.step(ctx, protos.AitoolsCommandStepFlagsValidated)
				ctx = withRerunInvocation(ctx, installInvocation{path: pathFlag, skills: skillsFlag, experimental: includeExperimental})
				written, err := installer.DumpSkillsToPath(ctx, src, pathFlag, opts)
				if err != nil {
					return withRecovery(ctx, err)
				}
				if written == 0 {
					// Every skill was filtered out (e.g. all experimental without
					// --experimental). The command still exits 0, but nothing was
					// delivered, so the funnel must not report SUCCESS.
					funnel.finishWith(ctx, protos.AitoolsCommandOutcomeFailure, protos.AitoolsErrorCategoryUncategorized)
					return nil
				}
				funnel.step(ctx, protos.AitoolsCommandStepSkillsDumped)
				return nil
			}

			projectFlag, globalFlag, err := parseScopeFlag(scopeFlag, projectFlag, globalFlag, false)
			if err != nil {
				return withCategory(err, protos.AitoolsErrorCategoryInvalidFlags)
			}

			// JSON output must be fully non-interactive: every choice has to come
			// from flags so no scope prompt, agent picker, or confirm is shown.
			// Require the flags those prompts would otherwise resolve, and fail
			// fast naming them.
			if jsonMode {
				var missing []string
				if !projectFlag && !globalFlag {
					missing = append(missing, "--scope")
				}
				if agentsFlag == "" {
					missing = append(missing, "--agents")
				}
				if len(missing) > 0 {
					return withCategory(fmt.Errorf("--output json requires %s so the command runs without interactive prompts", strings.Join(missing, " and ")), protos.AitoolsErrorCategoryInvalidFlags)
				}
			}
			// --project with --global is a flag error; check it here so it is not
			// reported as a scope prompt failure below.
			if _, err := resolveScope(projectFlag, globalFlag); err != nil {
				return withCategory(err, protos.AitoolsErrorCategoryInvalidFlags)
			}
			funnel.step(ctx, protos.AitoolsCommandStepFlagsValidated)

			interactive := installPromptsSupported(ctx)
			scope, err := resolveScopeWithPrompt(ctx, projectFlag, globalFlag, interactive)
			if err != nil {
				return withRecovery(ctx, promptError(err))
			}
			opts.Scope = scope
			funnel.setScope(scope)
			funnel.step(ctx, protos.AitoolsCommandStepScopeResolved)
			// Recovery steps repeat this run with the choices already made, so a
			// copied command never widens to every agent or the global scope.
			rerun := installInvocation{scope: scope, skills: skillsFlag, skillsOnly: skillsOnly, experimental: includeExperimental}
			ctx = withRerunInvocation(ctx, rerun)

			// Resolve the agents to act on.
			var targetAgents []*agents.Agent
			explicit := agentsFlag != ""
			if explicit {
				targetAgents, err = resolveAgentNames(ctx, agentsFlag)
				if err != nil {
					return err
				}
			} else {
				targetAgents, err = selectAgents(ctx, scope, skillsOnly, interactive)
				if err != nil {
					return withRecovery(ctx, promptError(err))
				}
				if len(targetAgents) == 0 {
					printNoAgentsMessage(ctx)
					funnel.finishWith(ctx, protos.AitoolsCommandOutcomeNothingToDo, protos.AitoolsErrorCategoryUnspecified)
					return nil
				}
			}
			// Name the agents the user chose, by flag or in the picker. Without
			// a choice the re-run detects the same agents again.
			if explicit || interactive {
				names := make([]string, len(targetAgents))
				for i, a := range targetAgents {
					names[i] = a.Name
				}
				rerun.agents = strings.Join(names, ",")
				ctx = withRerunInvocation(ctx, rerun)
			}
			funnel.setAgents(targetAgents)
			funnel.step(ctx, protos.AitoolsCommandStepAgentsSelected)

			plan := buildPlan(targetAgents, scope, skillsOnly, explicit)

			// The picker selection is the user's confirmation. A separate
			// "Proceed?" would only repeat it: agents that would be skipped are
			// not offered in the picker, so the plan holds no surprises.
			if !explicit && interactive {
				printPlanSummary(ctx, plan, scope)
			}
			funnel.step(ctx, protos.AitoolsCommandStepPlanConfirmed)

			var outcomes []agentOutcome
			var runErr error
			defer func() {
				logInstallEvent(ctx, plan, installOpts{
					Scope:        opts.Scope,
					Experimental: opts.IncludeExperimental,
				}, classifyInstallError(topLevelFailure(runErr)), outcomes)
			}()

			outcomes, runErr = executePlan(ctx, src, plan, opts, jsonMode)
			for _, s := range deliverySteps(outcomes) {
				funnel.step(ctx, s)
			}
			switch {
			case runErr == nil:
				if category, ok := nothingInstalledCategory(outcomes); ok {
					funnel.finishWith(ctx, protos.AitoolsCommandOutcomeFailure, category)
				} else if onlyKeptPlugins(outcomes) {
					funnel.finishWith(ctx, protos.AitoolsCommandOutcomeNothingToDo, protos.AitoolsErrorCategoryUnspecified)
				}
			case topLevelFailure(runErr) == nil:
				// Named agents failed or were skipped. A skip has no error type of
				// its own, so take the category from the outcomes, not runErr.
				funnel.finishWith(ctx, protos.AitoolsCommandOutcomeFailure, firstFailureCategory(outcomes))
			default:
				// Finish on the real failure here: JSON mode returns the
				// ErrAlreadyPrinted sentinel below.
				funnel.finish(ctx, runErr)
			}

			if jsonMode {
				if jerr := renderJSON(cmd.OutOrStdout(), buildInstallOutput(ctx, opts.Scope, outcomes, runErr)); jerr != nil {
					// Rendering failed, so the JSON the caller parses is broken.
					// Report the render error unless the run already failed for
					// another reason.
					if runErr == nil {
						runErr = jerr
					}
					return runErr
				}
				// The JSON payload is the only thing on stdout and already reports
				// the outcome. On failure, exit non-zero without root printing a
				// duplicate "Error: ..." line to stderr; root prints errors itself
				// (see cmd/root/root.go), so ErrAlreadyPrinted is how a command
				// opts out of that
				if runErr != nil {
					return root.ErrAlreadyPrinted
				}
				return nil
			}
			return withRecovery(ctx, runErr)
		},
	}

	cmd.Flags().StringVar(&skillsFlag, "skills", "", "Specific skills to install (comma-separated)")
	cmd.Flags().StringVar(&agentsFlag, "agents", "", "Agents to install for (comma-separated, e.g. claude-code,cursor)")
	cmd.Flags().BoolVar(&includeExperimental, "experimental", false, "Include experimental skills")
	cmd.Flags().BoolVar(&skillsOnly, "skills-only", false, "Force raw skill files for every agent instead of the plugin")
	cmd.Flags().StringVar(&pathFlag, "path", "", "Write resolved skill files to this directory (no agents, no state)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "", "Install scope: project or global (default: global, or prompt when interactive)")
	cmd.Flags().BoolVar(&projectFlag, "project", false, "Install to project directory (cwd)")
	cmd.Flags().BoolVar(&globalFlag, "global", false, "Install globally (default)")
	markScopeBoolsDeprecated(cmd)
	// Resolve auth best-effort and time-bounded so telemetry can upload.
	withTelemetryAuth(cmd)
	return cmd
}

// installOutputIsJSON reports whether --output json was requested. Unlike list,
// install can run detached from root: the legacy `skills install` alias builds a
// NewInstallCmd and executes it directly (see newLegacySkillsInstallCmd), so the
// root-supplied --output flag may be absent. Treat a missing flag as text rather
// than panicking the way root.OutputType would.
func installOutputIsJSON(cmd *cobra.Command) bool {
	f := cmd.Flag("output")
	if f == nil {
		return false
	}
	out, ok := f.Value.(*flags.Output)
	return ok && *out == flags.OutputJSON
}

// selectAgents returns the agents to act on when --agents is not given. The
// interactive path shows a picker over all known agents; the non-interactive
// path acts on detected agents, matching today's default. Skills delivery only
// needs a config dir, so in --skills-only mode an agent is "detected" by its
// config dir (PATH-independent); plugin delivery additionally detects agents by
// their CLI binary on PATH, which fixes the Codex/Copilot config-dir miss.
func selectAgents(ctx context.Context, scope string, skillsOnly, interactive bool) ([]*agents.Agent, error) {
	// Interactive: the picker decides; a prompt error or empty selection is a real
	// error, not a "nothing detected" no-op.
	if interactive {
		choices := agentChoices(ctx, scope, skillsOnly)
		if len(choices) == 0 {
			// Agents were detected but none can be acted on in this scope; the
			// caller prints the no-agents message rather than showing an empty picker.
			return nil, nil
		}
		return promptAgentSelection(ctx, choices)
	}

	var selected []*agents.Agent
	for _, a := range agents.Registry {
		detected := a.Detected(ctx)
		if !skillsOnly {
			detected = detected || a.HasBinary(ctx)
		}
		if detected {
			selected = append(selected, a)
		}
	}
	return selected, nil
}

// agentChoices builds the interactive picker rows. Every detected agent is shown
// in the detection list with its state, but only agents that can actually be
// acted on in the chosen scope (plugin or skills delivery) become selectable
// options. Agents that would be skipped (e.g. a files-only agent under project
// scope) are listed with their reason but are not checkboxes, so the picker never
// offers an option that does nothing.
func agentChoices(ctx context.Context, scope string, skillsOnly bool) []agentChoice {
	cmdio.LogString(ctx, "Detecting coding agents...")
	var choices []agentChoice
	for _, a := range agents.Registry {
		item := planItemFor(a, scope, skillsOnly, false)
		label := agentChoiceLabel(ctx, a, item)
		cmdio.LogString(ctx, fmt.Sprintf("  %-16s %s", a.DisplayName, label))
		if item.delivery == deliverySkip {
			continue
		}
		choices = append(choices, agentChoice{
			agent:     a,
			label:     a.DisplayName + "  " + label,
			preselect: a.IsPreselected(ctx),
		})
	}
	return choices
}

// agentChoiceLabel is the picker label: the detection state, plus the skip
// reason when the agent can't be acted on in the chosen scope.
func agentChoiceLabel(ctx context.Context, a *agents.Agent, item agentPlanItem) string {
	label := agentStateLabel(a.DisplayState(ctx))
	if item.delivery == deliverySkip {
		return label + " · " + item.reason
	}
	return label
}

// agentStateLabel is the short human label for a detection state.
func agentStateLabel(s agents.DisplayState) string {
	switch s {
	case agents.StateAvailable:
		return "plugin"
	case agents.StateInstalledCLIMissing:
		return "plugin · CLI not found"
	case agents.StateFilesOnly:
		return "skills"
	default:
		return "not found"
	}
}

func defaultPromptAgentSelection(_ context.Context, choices []agentChoice) ([]*agents.Agent, error) {
	options := make([]huh.Option[string], 0, len(choices))
	byName := make(map[string]*agents.Agent, len(choices))
	for _, c := range choices {
		options = append(options, huh.NewOption(c.label, c.agent.Name).Selected(c.preselect))
		byName[c.agent.Name] = c.agent
	}

	var selected []string
	err := huh.NewMultiSelect[string]().
		Title("Select agents to set up").
		Description("space to toggle, enter to confirm").
		Options(options...).
		Value(&selected).
		Run()
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, errNoAgentsSelected
	}

	result := make([]*agents.Agent, 0, len(selected))
	for _, name := range selected {
		result = append(result, byName[name])
	}
	return result, nil
}

// buildPlan resolves the per-agent delivery and scope. Plugin-first: an agent
// with a headless plugin install gets the plugin; --skills-only forces skills
// everywhere; agents with no plugin always get skills.
func buildPlan(targetAgents []*agents.Agent, scope string, skillsOnly, explicit bool) []agentPlanItem {
	plan := make([]agentPlanItem, 0, len(targetAgents))
	for _, a := range targetAgents {
		plan = append(plan, planItemFor(a, scope, skillsOnly, explicit))
	}
	return plan
}

// planItemFor resolves the delivery and scope for a single agent in the given
// install scope. It is shared by buildPlan and the interactive picker so the
// picker and the plan agree on what an agent will (or won't) do.
func planItemFor(a *agents.Agent, scope string, skillsOnly, explicit bool) agentPlanItem {
	item := agentPlanItem{agent: a, cliScope: scope, explicit: explicit}
	switch {
	case skillsOnly || a.Plugin == nil:
		// Raw-skills delivery (no-plugin agents, or --skills-only). Only some agents
		// support project-scoped skills, so skip the rest up front instead of
		// offering an option that fails at install time.
		if scope == installer.ScopeProject && !a.SupportsProjectScope {
			item.delivery = deliverySkip
			item.reason = "does not support project-scoped skills"
			item.skipErrorCategory = protos.AitoolsErrorCategoryUnsupportedScope
		} else {
			item.delivery = deliverySkills
		}
	default:
		nativeScope, ok, reason := mapAgentScope(a, scope)
		if !ok {
			item.delivery = deliverySkip
			item.reason = reason
			item.skipErrorCategory = protos.AitoolsErrorCategoryUnsupportedScope
		} else {
			item.delivery = deliveryPlugin
			item.scope = nativeScope
		}
	}
	return item
}

// printPlanSummary renders the interactive plan summary after the agent picker.
func printPlanSummary(ctx context.Context, plan []agentPlanItem, scope string) {
	cmdio.LogString(ctx, "")
	cmdio.LogString(ctx, "Plan ("+scope+" scope):")
	for _, it := range plan {
		switch it.delivery {
		case deliveryPlugin:
			cmdio.LogString(ctx, "  "+it.agent.DisplayName+"  install the databricks plugin")
		case deliverySkills:
			cmdio.LogString(ctx, "  "+it.agent.DisplayName+"  install skills")
		case deliverySkip:
			cmdio.LogString(ctx, "  "+it.agent.DisplayName+"  skip ("+it.reason+")")
		}
	}
	cmdio.LogString(ctx, "")
}

// agentOutcome is one agent's result after executePlan: how the databricks
// tools were delivered (or attempted), and, when the agent did not succeed, the
// failure category and a human-readable message for --output json. The category
// is what telemetry records; the message is local-only and never sent.
type agentOutcome struct {
	agent         *agents.Agent
	delivery      delivery
	status        outcomeStatus
	errorCategory protos.AitoolsErrorCategory // Unspecified when status == outcomeInstalled
	message       string                      // set when skipped or failed
	recovery      string                      // next step for the user, when skipped or failed
}

type outcomeStatus string

const (
	outcomeInstalled outcomeStatus = "installed"
	outcomeSkipped   outcomeStatus = "skipped"
	outcomeFailed    outcomeStatus = "failed"
	// outcomeKept is an agent whose plugin install could not run but which
	// already has the plugin: it is set up, though nothing was installed.
	outcomeKept outcomeStatus = "kept"
)

// agentErrors wraps the failures of explicitly named agents, which are already
// reported in their per-agent outcomes. Wrapping lets the JSON and telemetry
// layers tell a per-agent failure apart from a top-level failure that has no
// per-agent entry, so each is surfaced exactly once.
type agentErrors struct{ err error }

func (e *agentErrors) Error() string { return e.err.Error() }
func (e *agentErrors) Unwrap() error { return e.err }

// topLevelFailure returns the run error when it has no per-agent entry, or nil
// when the failure is already reported per agent — so a per-agent failure is not
// duplicated in the top-level error/errorCategory fields.
func topLevelFailure(runErr error) error {
	if _, ok := errors.AsType[*agentErrors](runErr); ok {
		return nil
	}
	return runErr
}

// executePlan carries out the plan and returns each agent's outcome. Skills
// installs go through the existing skills path. Plugin installs are reported but
// never silently fall back to skills: a blocked install is a warning, unless the
// agent was explicitly named via --agents, which is an error. A run that sets up
// no agent at all is an error too, so callers never mistake it for success.
func executePlan(ctx context.Context, src installer.ManifestSource, plan []agentPlanItem, opts installer.InstallOptions, quiet bool) ([]agentOutcome, error) {
	var skillsAgents []*agents.Agent
	var pluginItems, skipItems []agentPlanItem
	for _, it := range plan {
		switch it.delivery {
		case deliverySkills:
			skillsAgents = append(skillsAgents, it.agent)
		case deliveryPlugin:
			pluginItems = append(pluginItems, it)
		case deliverySkip:
			skipItems = append(skipItems, it)
		}
	}

	var outcomes []agentOutcome
	var explicitErrs []error

	// A skills install runs as a group; its failure is the command's top-level
	// error. Plugin agents don't depend on it, so they are still attempted.
	var skillsErr error
	if len(skillsAgents) > 0 {
		if !quiet {
			installer.PrintInstallingFor(ctx, skillsAgents)
		}
		skillsErr = installSkillsForAgentsFn(ctx, src, skillsAgents, opts)
		if skillsErr == nil {
			for _, a := range skillsAgents {
				outcomes = append(outcomes, agentOutcome{agent: a, delivery: deliverySkills, status: outcomeInstalled})
			}
		}
	}

	pluginCount := 0
	var fallbackAgents []*agents.Agent
	if len(pluginItems) > 0 {
		ref, err := installer.GetPluginSkillsRef(ctx)
		if err != nil {
			return outcomes, errors.Join(skillsErr, err)
		}
		records := map[string]installer.PluginRecord{}
		for _, it := range pluginItems {
			if !quiet {
				cmdio.LogString(ctx, fmt.Sprintf("Installing databricks plugin for %s...", it.agent.DisplayName))
			}
			rec, err := installPluginForAgentFn(ctx, it.agent, it.scope, ref)
			if err != nil && fallsBackToSkills(it, err) && pluginAlreadyInstalledFn(ctx, it.agent, opts.Scope) {
				// Raw skills next to the plugin would show every skill twice, and
				// `aitools update` never reconciles raw files on a plugin agent.
				if !quiet {
					cmdio.LogString(ctx, cmdio.Yellow(ctx, pluginKeptText(ctx, it, err)))
				}
				outcomes = append(outcomes, agentOutcome{
					agent:         it.agent,
					delivery:      deliveryPlugin,
					status:        outcomeKept,
					errorCategory: classifyInstallError(err),
					message:       err.Error(),
				})
				continue
			}
			if err != nil && fallsBackToSkills(it, err) {
				if !quiet {
					cmdio.LogString(ctx, cmdio.Yellow(ctx, skillsFallbackText(ctx, it, err)))
				}
				// Keep the plugin failure in the outcomes so telemetry still sees
				// it; the skills outcome added below records the fallback.
				outcomes = append(outcomes, agentOutcome{
					agent:         it.agent,
					delivery:      deliveryPlugin,
					status:        outcomeFailed,
					errorCategory: classifyInstallError(err),
					message:       err.Error(),
				})
				fallbackAgents = append(fallbackAgents, it.agent)
				continue
			}
			if err != nil {
				reason, recovery := pluginFailureText(ctx, it, err)
				if !quiet {
					cmdio.LogString(ctx, cmdio.Yellow(ctx, fmt.Sprintf("Skipped %s: %s", it.agent.DisplayName, reason)))
					cmdio.LogString(ctx, "  "+recovery)
				}
				outcomes = append(outcomes, agentOutcome{
					agent:         it.agent,
					delivery:      deliveryPlugin,
					status:        outcomeFailed,
					errorCategory: classifyInstallError(err),
					message:       err.Error(),
					recovery:      recovery,
				})
				if it.explicit {
					explicitErrs = append(explicitErrs, &displayError{msg: it.agent.DisplayName + ": " + reason, err: err})
				}
				continue
			}
			records[it.agent.Name] = rec
			pluginCount++
			outcomes = append(outcomes, agentOutcome{agent: it.agent, delivery: deliveryPlugin, status: outcomeInstalled})
			// Remove any raw skills we previously dropped on this agent so the
			// plugin and leftover files don't surface the same skills twice.
			if err := cleanupLegacyFn(ctx, it.agent, opts.Scope); err != nil {
				log.Debugf(ctx, "Legacy skill cleanup for %s failed: %v", it.agent.DisplayName, err)
			}
			if !quiet {
				cmdio.LogString(ctx, fmt.Sprintf("  %s  databricks plugin %s", it.agent.DisplayName, versionToken(rec.Version)))
			}
		}
		if len(records) > 0 {
			if err := recordPluginInstallsFn(ctx, opts.Scope, records, ref); err != nil {
				return outcomes, errors.Join(skillsErr, err)
			}
		}
	}

	if len(fallbackAgents) > 0 {
		if !quiet {
			installer.PrintInstallingFor(ctx, fallbackAgents)
		}
		if err := installSkillsForAgentsFn(ctx, src, fallbackAgents, opts); err != nil {
			skillsErr = errors.Join(skillsErr, err)
		} else {
			for _, a := range fallbackAgents {
				outcomes = append(outcomes, agentOutcome{agent: a, delivery: deliverySkills, status: outcomeInstalled})
			}
			// These agents have no plugin (pluginAlreadyInstalled said so), so a
			// plugin record left by a removal with the agent's own CLI is stale.
			// Drop it, or list and update would keep treating them as plugin
			// agents and never refresh their skill files.
			names := make([]string, len(fallbackAgents))
			for i, a := range fallbackAgents {
				names[i] = a.Name
			}
			if err := installer.ForgetPluginInstalls(ctx, opts.Scope, names); err != nil {
				skillsErr = errors.Join(skillsErr, err)
			}
		}
	}

	for _, it := range skipItems {
		recovery := skipRecovery(it)
		if !quiet {
			cmdio.LogString(ctx, cmdio.Yellow(ctx, "Skipped "+it.agent.DisplayName+": "+it.reason))
			cmdio.LogString(ctx, "  "+recovery)
		}
		outcomes = append(outcomes, agentOutcome{
			agent:         it.agent,
			delivery:      deliverySkip,
			status:        outcomeSkipped,
			errorCategory: it.skipErrorCategory,
			message:       it.reason,
			recovery:      recovery,
		})
		if it.explicit {
			explicitErrs = append(explicitErrs, fmt.Errorf("%s: %s", it.agent.DisplayName, it.reason))
		}
	}

	if pluginCount > 0 && !quiet {
		noun := "agent"
		if pluginCount != 1 {
			noun = "agents"
		}
		cmdio.LogString(ctx, fmt.Sprintf("Installed the plugin for %d %s.", pluginCount, noun))
	}

	// The skills error has no per-agent entry, so it stays the top-level error;
	// explicit per-agent failures are already in their outcomes.
	if skillsErr != nil {
		return outcomes, skillsErr
	}
	if len(explicitErrs) > 0 {
		return outcomes, &agentErrors{err: errors.Join(explicitErrs...)}
	}
	if !slices.ContainsFunc(outcomes, func(o agentOutcome) bool { return o.status == outcomeInstalled || o.status == outcomeKept }) {
		return outcomes, &agentErrors{err: newNothingInstalledError(outcomes)}
	}
	return outcomes, nil
}

type installOutput struct {
	Scope  string            `json:"scope"`
	Agents []agentResultJSON `json:"agents"`

	// Error and ErrorCategory describe a top-level failure with no per-agent
	// entry (e.g. a skills-group install failure); both empty on success. Error
	// is the local-only message; ErrorCategory is the classification telemetry
	// also records.
	Error         string `json:"error,omitempty"`
	ErrorCategory string `json:"error_category,omitempty"`
	// Recovery is the next step for the top-level failure, local-only like Error.
	Recovery string `json:"recovery,omitempty"`
}

type agentResultJSON struct {
	Name          string `json:"name"`
	Delivery      string `json:"delivery"`
	Status        string `json:"status"`
	ErrorCategory string `json:"error_category,omitempty"`
	Message       string `json:"message,omitempty"`
	Recovery      string `json:"recovery,omitempty"`
}

func buildInstallOutput(ctx context.Context, scope string, outcomes []agentOutcome, runErr error) installOutput {
	out := installOutput{Scope: scope, Agents: make([]agentResultJSON, 0, len(outcomes))}
	for _, o := range outcomes {
		entry := agentResultJSON{
			Name:     o.agent.Name,
			Delivery: o.delivery.String(),
			Status:   string(o.status),
			Message:  o.message,
			Recovery: o.recovery,
		}
		// Only non-successful outcomes carry a category, keyed on status to match
		// agentResultsField in telemetry.go so the JSON and telemetry views of the
		// same slice never disagree.
		if o.status != outcomeInstalled {
			entry.ErrorCategory = string(o.errorCategory)
		}
		out.Agents = append(out.Agents, entry)
	}
	// A top-level failure (skills-group install, ref lookup, plugin recording)
	// has no per-agent entry, so surface it here too; otherwise the consumer sees
	// a non-zero exit with an empty agents array and no reason. Per-agent failures
	// stay in the agents entries above and are not repeated here.
	if e := topLevelFailure(runErr); e != nil {
		out.Error = e.Error()
		out.ErrorCategory = string(classifyInstallError(e))
		out.Recovery = installRecovery(ctx, e)
	}
	return out
}

// resolveAgentNames parses a comma-separated list of agent names and validates
// them against the registry. Returns an error for unrecognized names.
func resolveAgentNames(_ context.Context, names string) ([]*agents.Agent, error) {
	available := make(map[string]*agents.Agent, len(agents.Registry))
	var availableNames []string
	for _, a := range agents.Registry {
		available[a.Name] = a
		availableNames = append(availableNames, a.Name)
	}

	var result []*agents.Agent
	seen := make(map[string]bool)
	for name := range strings.SplitSeq(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		agent, ok := available[name]
		if !ok {
			return nil, withCategory(fmt.Errorf("unknown agent %q. Available agents: %s", name, strings.Join(availableNames, ", ")), protos.AitoolsErrorCategoryUnknownAgent)
		}
		result = append(result, agent)
	}

	if len(result) == 0 {
		return nil, withCategory(errors.New("no agents specified"), protos.AitoolsErrorCategoryInvalidFlags)
	}
	return result, nil
}

// printNoAgentsMessage prints the "no agents detected" message.
func printNoAgentsMessage(ctx context.Context) {
	cmdio.LogString(ctx, cmdio.Yellow(ctx, "No supported coding agents found on PATH."))
	cmdio.LogString(ctx, "")
	cmdio.LogString(ctx, "Supported: "+strings.Join(agents.SupportedNames(), ", ")+".")
	cmdio.LogString(ctx, "Install one, then re-run 'databricks aitools install'. To set up an agent that was not detected, name it: databricks aitools install --agents=<name>")
}
