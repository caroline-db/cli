package aitools

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/databricks/cli/cmd/root"
	"github.com/databricks/cli/internal/testutil"
	"github.com/databricks/cli/libs/aitools/agents"
	"github.com/databricks/cli/libs/aitools/installer"
	"github.com/databricks/cli/libs/cmdio"
	"github.com/databricks/cli/libs/telemetry"
	"github.com/databricks/cli/libs/telemetry/protos"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateAgents pins the calling agent with testutil.SetAgentEnv and clears
// the config-dir overrides agent detection reads, so a host CLAUDE_CONFIG_DIR,
// GEMINI_CLI_HOME, APPDATA (Windows) etc. cannot add agents to a test that
// builds its own temp HOME.
func isolateAgents(t *testing.T, kv ...string) {
	t.Helper()
	testutil.SetAgentEnv(t, kv...)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "GEMINI_CLI_HOME", "GOOSE_PATH_ROOT", "APPDATA"} {
		t.Setenv(v, "")
	}
}

// forbidSkillsInstall fails the test if the run reaches the real skills
// installer, which would fetch skills and write into agent config dirs.
func forbidSkillsInstall(t *testing.T) {
	t.Helper()
	orig := installSkillsForAgentsFn
	t.Cleanup(func() { installSkillsForAgentsFn = orig })
	installSkillsForAgentsFn = func(_ context.Context, _ installer.ManifestSource, targets []*agents.Agent, _ installer.InstallOptions) error {
		t.Errorf("unexpected skills install for %d agents", len(targets))
		return assert.AnError
	}
}

// funnelEvents returns the AitoolsCommandEvents buffered on ctx, in order.
func funnelEvents(ctx context.Context) []protos.AitoolsCommandEvent {
	var out []protos.AitoolsCommandEvent
	for _, e := range telemetry.BufferedEvents(ctx) {
		if e.AitoolsCommandEvent != nil {
			out = append(out, *e.AitoolsCommandEvent)
		}
	}
	return out
}

// funnelShape renders each event as its stage plus step or outcome, so a test
// can assert the whole sequence in one line.
func funnelShape(events []protos.AitoolsCommandEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		switch e.Stage {
		case protos.AitoolsCommandStageStepCompleted:
			out = append(out, string(e.Step))
		case protos.AitoolsCommandStageFinished:
			out = append(out, string(e.Stage)+":"+string(e.Outcome)+":"+string(e.ErrorCategory))
		default:
			out = append(out, string(e.Stage))
		}
	}
	return out
}

func runInstall(t *testing.T, ctx context.Context, args ...string) (context.Context, error) {
	ctx = telemetry.WithNewLogger(ctx)
	cmd := newTestInstallCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetArgs(args)
	return ctx, cmd.Execute()
}

func TestInstallFunnelSkillsOnlySuccess(t *testing.T) {
	isolateAgents(t, "CURSOR_AGENT", "1")
	setupTestAgents(t)
	setupInstallMock(t)

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), "--skills-only")
	require.NoError(t, err)

	events := funnelEvents(ctx)
	assert.Equal(t, []string{
		"STARTED",
		"FLAGS_VALIDATED",
		"SCOPE_RESOLVED",
		"AGENTS_SELECTED",
		"PLAN_CONFIRMED",
		"SKILLS_INSTALLED",
		"FINISHED:SUCCESS:",
	}, funnelShape(events))

	for _, e := range events {
		assert.Equal(t, protos.AitoolsCommandInstall, e.Command)
		assert.Equal(t, protos.AitoolsInstallModeSkillsOnly, e.InstallMode)
		assert.False(t, e.Interactive)
	}
	finished := events[len(events)-1]
	assert.Equal(t, protos.AitoolsInstallScopeGlobal, finished.Scope)
	assert.Equal(t, []protos.AitoolsAgentType{protos.AitoolsAgentTypeClaudeCode, protos.AitoolsAgentTypeCursor}, finished.Agents)

	// The install event is still emitted next to the funnel.
	var installEvents int
	for _, e := range telemetry.BufferedEvents(ctx) {
		if e.AitoolsInstallEvent != nil {
			installEvents++
		}
	}
	assert.Equal(t, 1, installEvents)
}

func TestInstallFunnelEarlyExits(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "conflicting delivery flags",
			args: []string{"--skills-only", "--path", "out"},
			want: []string{"STARTED", "FINISHED:FAILURE:INVALID_FLAGS"},
		},
		{
			name: "skills without skills-only",
			args: []string{"--skills", "databricks"},
			want: []string{"STARTED", "FINISHED:FAILURE:INVALID_FLAGS"},
		},
		{
			name: "bad scope",
			args: []string{"--scope", "nowhere"},
			want: []string{"STARTED", "FINISHED:FAILURE:INVALID_FLAGS"},
		},
		{
			name: "json without agents",
			args: []string{"--output", "json", "--scope", "global"},
			want: []string{"STARTED", "FINISHED:FAILURE:INVALID_FLAGS"},
		},
		{
			name: "unknown agent",
			args: []string{"--agents", "no-such-agent"},
			want: []string{"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "FINISHED:FAILURE:UNKNOWN_AGENT"},
		},
		{
			name: "project and global",
			args: []string{"--project", "--global"},
			want: []string{"STARTED", "FINISHED:FAILURE:INVALID_FLAGS"},
		},
		{
			name: "empty agents list",
			args: []string{"--agents", ","},
			want: []string{"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "FINISHED:FAILURE:INVALID_FLAGS"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAgents(t)
			setupTestAgents(t)
			ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), tc.args...)
			require.Error(t, err)
			assert.Equal(t, tc.want, funnelShape(funnelEvents(ctx)))
		})
	}
}

func TestInstallFunnelNoAgentsDetected(t *testing.T) {
	isolateAgents(t)
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp+"/.config")
	fakeBinsOnPath(t)
	forbidSkillsInstall(t)

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "FINISHED:NOTHING_TO_DO:",
	}, funnelShape(funnelEvents(ctx)))
}

func TestInstallFunnelPluginFailureThroughJSON(t *testing.T) {
	isolateAgents(t, "CODEX_CI", "1")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	fakeBinsOnPath(t, "codex")
	t.Setenv("DATABRICKS_SKILLS_REF", "v0.2.6")

	origInstall := installPluginForAgentFn
	t.Cleanup(func() { installPluginForAgentFn = origInstall })
	installPluginForAgentFn = func(_ context.Context, a *agents.Agent, _, _ string) (installer.PluginRecord, error) {
		return installer.PluginRecord{}, &installer.BlockedError{Agent: a.Name, Reason: installer.ReasonCLINotOnPath}
	}

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), "--agents", "codex", "--scope", "global", "--output", "json")
	require.ErrorIs(t, err, root.ErrAlreadyPrinted)

	// A named agent's failure finishes with that agent's category, not the
	// ErrAlreadyPrinted sentinel the command returns in JSON mode.
	assert.Equal(t, []string{
		"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "AGENTS_SELECTED", "PLAN_CONFIRMED",
		"FINISHED:FAILURE:CLI_NOT_ON_PATH",
	}, funnelShape(funnelEvents(ctx)))
}

// A failed skills install is a top-level failure, so in JSON mode it reaches
// the ErrAlreadyPrinted return. FINISHED must classify the install error, not
// the sentinel (which would be UNCATEGORIZED).
func TestInstallFunnelSkillsFailureThroughJSON(t *testing.T) {
	isolateAgents(t)
	setupTestAgents(t)
	orig := installSkillsForAgentsFn
	t.Cleanup(func() { installSkillsForAgentsFn = orig })
	installSkillsForAgentsFn = func(context.Context, installer.ManifestSource, []*agents.Agent, installer.InstallOptions) error {
		return fmt.Errorf("fetch skills: %w", context.DeadlineExceeded)
	}

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), "--agents", "cursor", "--skills-only", "--scope", "global", "--output", "json")
	require.ErrorIs(t, err, root.ErrAlreadyPrinted)
	assert.Equal(t, []string{
		"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "AGENTS_SELECTED", "PLAN_CONFIRMED",
		"FINISHED:FAILURE:TIMEOUT",
	}, funnelShape(funnelEvents(ctx)))
}

func TestInstallFunnelInteractiveCancel(t *testing.T) {
	cases := []struct {
		name    string
		proceed func() (bool, error)
		wantErr bool
		want    string
	}{
		{name: "declined", proceed: func() (bool, error) { return false, nil }, want: "FINISHED:CANCELLED:"},
		{name: "aborted", proceed: func() (bool, error) { return false, huh.ErrUserAborted }, wantErr: true, want: "FINISHED:CANCELLED:USER_ABORTED"},
		{name: "prompt failed", proceed: func() (bool, error) { return false, assert.AnError }, wantErr: true, want: "FINISHED:FAILURE:PROMPT_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAgents(t)
			setupTestAgents(t)
			setupScopeMock(t, installer.ScopeGlobal)
			origPicker, origProceed := promptAgentSelection, promptProceed
			t.Cleanup(func() { promptAgentSelection, promptProceed = origPicker, origProceed })
			promptAgentSelection = func(_ context.Context, choices []agentChoice) ([]*agents.Agent, error) {
				return []*agents.Agent{choices[0].agent}, nil
			}
			promptProceed = tc.proceed

			ioCtx, test := cmdio.SetupTest(t.Context(), cmdio.TestOptions{PromptSupported: true})
			defer test.Done()
			go drainReader(test.Stdout)
			go drainReader(test.Stderr)

			ctx := telemetry.WithNewLogger(ioCtx)
			cmd := newTestInstallCmd()
			cmd.SetContext(ctx)
			err := cmd.RunE(cmd, nil)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			events := funnelEvents(ctx)
			assert.Equal(t, []string{"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "AGENTS_SELECTED", tc.want}, funnelShape(events))
			assert.True(t, events[0].Interactive)
		})
	}
}

// Confirming the picker with nothing checked is the user opting out, so it
// finishes CANCELLED like a declined confirm, though the command still errors.
func TestInstallFunnelEmptyPickerSelectionIsCancelled(t *testing.T) {
	isolateAgents(t)
	setupTestAgents(t)
	setupScopeMock(t, installer.ScopeGlobal)
	origPicker := promptAgentSelection
	t.Cleanup(func() { promptAgentSelection = origPicker })
	promptAgentSelection = func(context.Context, []agentChoice) ([]*agents.Agent, error) {
		return nil, errNoAgentsSelected
	}

	ioCtx, test := cmdio.SetupTest(t.Context(), cmdio.TestOptions{PromptSupported: true})
	defer test.Done()
	go drainReader(test.Stdout)
	go drainReader(test.Stderr)

	ctx := telemetry.WithNewLogger(ioCtx)
	cmd := newTestInstallCmd()
	cmd.SetContext(ctx)
	err := cmd.RunE(cmd, nil)
	require.ErrorIs(t, err, errNoAgentsSelected)
	assert.Equal(t, "at least one agent must be selected", err.Error())
	assert.Equal(t, []string{"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "FINISHED:CANCELLED:"}, funnelShape(funnelEvents(ctx)))
}

// A named agent skipped for the scope fails the run with an error that has no
// type of its own; the funnel takes the category from the agent's outcome.
func TestInstallFunnelNamedAgentSkippedForScope(t *testing.T) {
	isolateAgents(t)
	setupTestAgents(t)
	setupInstallMock(t)

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), "--agents", "cursor", "--skills-only", "--scope", "project")
	require.ErrorContains(t, err, "does not support project-scoped skills")
	assert.Equal(t, []string{
		"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "AGENTS_SELECTED", "PLAN_CONFIRMED",
		"FINISHED:FAILURE:UNSUPPORTED_SCOPE",
	}, funnelShape(funnelEvents(ctx)))
}

func TestInstallFunnelPathDump(t *testing.T) {
	isolateAgents(t)
	setupTestAgents(t)
	// An unreachable manifest makes the dump fail after flag validation.
	t.Setenv("DATABRICKS_SKILLS_REF", "v0.2.6")
	t.Setenv("DATABRICKS_SKILLS_BASE_URL", "http://127.0.0.1:1")

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), "--path", t.TempDir())
	require.Error(t, err)
	events := funnelEvents(ctx)
	assert.Equal(t, []string{"STARTED", "FLAGS_VALIDATED", "FINISHED:FAILURE:NETWORK"}, funnelShape(events))
	assert.Equal(t, protos.AitoolsInstallModePath, events[0].InstallMode)
}

func TestInstrumentedCommandsLogStartAndFinish(t *testing.T) {
	isolateCommandEnv(t)

	ctx := telemetry.WithNewLogger(cmdio.MockDiscard(t.Context()))
	cmd := NewVersionCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())

	events := funnelEvents(ctx)
	assert.Equal(t, []string{"STARTED", "FINISHED:SUCCESS:"}, funnelShape(events))
	for _, e := range events {
		assert.Equal(t, protos.AitoolsCommandVersion, e.Command)
		assert.Empty(t, e.InstallMode)
	}
}

// Every instrumented command must resolve auth best-effort, or its events never
// upload (telemetry.Upload skips a command without auth config).
func TestInstrumentedCommandsResolveAuth(t *testing.T) {
	for _, cmd := range []*cobra.Command{NewInstallCmd(), NewUpdateCmd(), NewListCmd(), NewVersionCmd(), NewUninstallCmd()} {
		assert.NotNil(t, cmd.PreRunE, cmd.Name())
	}
}

func TestListFunnelFailure(t *testing.T) {
	isolateCommandEnv(t)
	t.Setenv("DATABRICKS_SKILLS_REF", "v0.2.6")
	t.Setenv("DATABRICKS_SKILLS_BASE_URL", "http://127.0.0.1:1")

	ctx := telemetry.WithNewLogger(cmdio.MockDiscard(t.Context()))
	cmd := NewListCmd()
	cmd.SetContext(ctx)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	require.Error(t, cmd.RunE(cmd, nil))
	assert.Equal(t, []string{"STARTED", "FINISHED:FAILURE:NETWORK"}, funnelShape(funnelEvents(ctx)))
}

// A plugin failure for an auto-detected agent is only a warning (exit 0), but
// when nothing got installed the funnel must not report SUCCESS.
func TestInstallFunnelDetectedPluginFailureIsNotSuccess(t *testing.T) {
	isolateAgents(t)
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp+"/.config")
	fakeBinsOnPath(t, "codex")
	t.Setenv("DATABRICKS_SKILLS_REF", "v0.2.6")
	setupPluginMock(t)
	forbidSkillsInstall(t)
	installPluginForAgentFn = func(_ context.Context, a *agents.Agent, _, _ string) (installer.PluginRecord, error) {
		return installer.PluginRecord{}, &installer.BlockedError{Agent: a.Name, Reason: installer.ReasonInstallFailed}
	}

	ctx, err := runInstall(t, cmdio.MockDiscard(t.Context()), "--scope", "global")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"STARTED", "FLAGS_VALIDATED", "SCOPE_RESOLVED", "AGENTS_SELECTED", "PLAN_CONFIRMED",
		"FINISHED:FAILURE:PLUGIN_INSTALL_FAILED",
	}, funnelShape(funnelEvents(ctx)))
}

func TestNothingInstalledCategory(t *testing.T) {
	codex := &agents.Agent{Name: agents.NameCodex}
	cursor := &agents.Agent{Name: agents.NameCursor}
	failed := agentOutcome{agent: codex, delivery: deliveryPlugin, status: outcomeFailed, errorCategory: protos.AitoolsErrorCategoryPluginInstallFailed}
	skipped := agentOutcome{agent: cursor, delivery: deliverySkip, status: outcomeSkipped, errorCategory: protos.AitoolsErrorCategoryUnsupportedScope}
	installed := agentOutcome{agent: cursor, delivery: deliverySkills, status: outcomeInstalled}

	_, ok := nothingInstalledCategory(nil)
	assert.False(t, ok)
	_, ok = nothingInstalledCategory([]agentOutcome{failed, installed})
	assert.False(t, ok, "one installed agent makes the run a success")
	got, ok := nothingInstalledCategory([]agentOutcome{failed, skipped})
	assert.True(t, ok)
	assert.Equal(t, protos.AitoolsErrorCategoryPluginInstallFailed, got)

	uncategorized := agentOutcome{agent: codex, delivery: deliveryPlugin, status: outcomeFailed}
	assert.Equal(t, protos.AitoolsErrorCategoryUnsupportedScope, firstFailureCategory([]agentOutcome{installed, uncategorized, skipped}))
	assert.Equal(t, protos.AitoolsErrorCategoryUncategorized, firstFailureCategory([]agentOutcome{installed, uncategorized}))

	assert.Equal(t, []protos.AitoolsCommandStep{protos.AitoolsCommandStepSkillsInstalled}, deliverySteps([]agentOutcome{failed, installed}))
	assert.Equal(t, []protos.AitoolsCommandStep{protos.AitoolsCommandStepPluginsInstalled},
		deliverySteps([]agentOutcome{failed, {agent: codex, delivery: deliveryPlugin, status: outcomeInstalled}}))
}

// isolateCommandEnv gives a full command run an empty HOME and cwd, no calling
// agent (isolateAgents), the remaining config dir overrides derived from that
// HOME, and a no-op telemetry auth resolver, so it reads no host state and
// contacts nothing.
func isolateCommandEnv(t *testing.T) {
	isolateAgents(t)
	noTelemetryAuth(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AIDEVKIT_HOME", "")
	t.Chdir(t.TempDir())
}

func TestUninstallDeclinedIsCancelled(t *testing.T) {
	isolateCommandEnv(t)
	globalDir, err := installer.GlobalSkillsDir(t.Context())
	require.NoError(t, err)
	require.NoError(t, installer.SaveState(globalDir, &installer.InstallState{
		SchemaVersion: 1,
		Release:       "v0.2.6",
		Skills:        map[string]string{"databricks-test": "1.0.0"},
	}))
	calls := setupUninstallMock(t)

	ioCtx, test := cmdio.SetupTest(t.Context(), cmdio.TestOptions{PromptSupported: true})
	defer test.Done()
	go drainReader(test.Stdout)
	go drainReader(test.Stderr)
	go func() {
		_, _ = test.Stdin.WriteString("n\n")
		_ = test.Stdin.Flush()
	}()

	ctx := telemetry.WithNewLogger(ioCtx)
	cmd := NewUninstallCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())

	assert.Empty(t, *calls)
	events := funnelEvents(ctx)
	assert.Equal(t, []string{"STARTED", "FINISHED:CANCELLED:"}, funnelShape(events))
	assert.True(t, events[0].Interactive)

	// Outside a funnel it is a no-op.
	assert.NotPanics(t, func() { funnelCancelled(t.Context()) })
}

// Update and uninstall with nothing installed keep their error (and exit code)
// but report NOTHING_TO_DO, not an uncategorized failure.
func TestNothingInstalledFinishesNothingToDo(t *testing.T) {
	const projectNotInstalled = "no project-scoped skills found in the current directory."
	cases := []struct {
		name    string
		newCmd  func() *cobra.Command
		args    []string
		command protos.AitoolsCommand
		wantErr string
	}{
		{name: "update", newCmd: NewUpdateCmd, command: protos.AitoolsCommandUpdate, wantErr: "no skills or plugins installed. Run 'databricks aitools install' to install"},
		{name: "uninstall", newCmd: NewUninstallCmd, command: protos.AitoolsCommandUninstall, wantErr: "no skills or plugins installed"},
		{name: "update project scope", newCmd: NewUpdateCmd, args: []string{"--scope=project"}, command: protos.AitoolsCommandUpdate, wantErr: projectNotInstalled},
		{name: "uninstall project scope", newCmd: NewUninstallCmd, args: []string{"--scope=project"}, command: protos.AitoolsCommandUninstall, wantErr: projectNotInstalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCommandEnv(t)
			// Nothing may be fetched; an unreachable source makes any fetch fail.
			t.Setenv("DATABRICKS_SKILLS_REF", "v0.2.6")
			t.Setenv("DATABRICKS_SKILLS_BASE_URL", "http://127.0.0.1:1")

			ctx := telemetry.WithNewLogger(cmdio.MockDiscard(t.Context()))
			cmd := tc.newCmd()
			cmd.SetContext(ctx)
			cmd.SetArgs(append([]string{}, tc.args...))
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			// The exact project-scope text is pinned by TestScopeNotInstalledError*.
			err := cmd.Execute()
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), tc.wantErr), err.Error())
			if tc.args == nil {
				assert.EqualError(t, err, tc.wantErr)
			}
			assert.ErrorIs(t, err, installer.ErrNothingInstalled)

			events := funnelEvents(ctx)
			assert.Equal(t, []string{"STARTED", "FINISHED:NOTHING_TO_DO:"}, funnelShape(events))
			for _, e := range events {
				assert.Equal(t, tc.command, e.Command)
			}
		})
	}
}

// Scope flag errors of update, list and uninstall finish INVALID_FLAGS, as
// install's do, and keep their message.
func TestScopeFlagErrorsFinishInvalidFlags(t *testing.T) {
	cases := []struct {
		name          string
		newCmd        func() *cobra.Command
		args          []string
		bothInstalled bool
		wantErr       string
	}{
		{name: "list invalid scope", newCmd: NewListCmd, args: []string{"--scope=bogus"}, wantErr: `invalid --scope "bogus": must be one of project, global, both`},
		{name: "list project and global", newCmd: NewListCmd, args: []string{"--project", "--global"}, wantErr: "cannot use --global and --project together"},
		{name: "update scope with project", newCmd: NewUpdateCmd, args: []string{"--scope=global", "--project"}, wantErr: "cannot use --scope with --project or --global; --project and --global are deprecated aliases for --scope"},
		{name: "update both scopes installed", newCmd: NewUpdateCmd, bothInstalled: true, wantErr: "skills are installed in both global and project scopes; use --scope=global, --scope=project, or --scope=both to specify which to update"},
		{name: "uninstall scope both", newCmd: NewUninstallCmd, args: []string{"--scope=both"}, wantErr: "--scope=both is not supported for this command; use 'project' or 'global'"},
		{name: "uninstall project and global", newCmd: NewUninstallCmd, args: []string{"--project", "--global"}, wantErr: "cannot uninstall both scopes at once; run uninstall separately with --scope=global and --scope=project"},
		{name: "uninstall both scopes installed", newCmd: NewUninstallCmd, bothInstalled: true, wantErr: "skills are installed in both global and project scopes; use --scope=global or --scope=project to specify which to uninstall"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCommandEnv(t)
			// Nothing may be fetched; an unreachable source makes any fetch fail.
			t.Setenv("DATABRICKS_SKILLS_REF", "v0.2.6")
			t.Setenv("DATABRICKS_SKILLS_BASE_URL", "http://127.0.0.1:1")
			if tc.bothInstalled {
				globalDir, err := installer.GlobalSkillsDir(t.Context())
				require.NoError(t, err)
				projectDir, err := installer.ProjectSkillsDir(t.Context())
				require.NoError(t, err)
				for _, dir := range []string{globalDir, projectDir} {
					require.NoError(t, installer.SaveState(dir, &installer.InstallState{SchemaVersion: 2, Release: "v0.2.6", Skills: map[string]string{"databricks": "0.2.6"}}))
				}
			}

			ctx := telemetry.WithNewLogger(cmdio.MockDiscard(t.Context()))
			cmd := tc.newCmd()
			cmd.SetContext(ctx)
			cmd.SetArgs(append([]string{}, tc.args...))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			assert.EqualError(t, cmd.Execute(), tc.wantErr)
			assert.Equal(t, []string{"STARTED", "FINISHED:FAILURE:INVALID_FLAGS"}, funnelShape(funnelEvents(ctx)))
		})
	}
}

var errCodexNotOnPath = &installer.BlockedError{Agent: agents.NameCodex, Reason: installer.ReasonCLINotOnPath}

// testPluginRecords are the plugins saveGlobalState records: Codex and Claude Code.
var testPluginRecords = map[string]installer.PluginRecord{
	agents.NameCodex:      {Plugin: "databricks", Version: "0.2.6"},
	agents.NameClaudeCode: {Marketplace: "claude-plugins-official", Plugin: "databricks", Version: "0.2.6"},
}

// saveGlobalState records a global install of testPluginRecords plus the given
// file skills (none for a plugin-only install), and returns its dir.
func saveGlobalState(t *testing.T, skills map[string]string) string {
	dir, err := installer.GlobalSkillsDir(t.Context())
	require.NoError(t, err)
	require.NoError(t, installer.SaveState(dir, &installer.InstallState{
		SchemaVersion: 2,
		Release:       "v0.2.6",
		Skills:        skills,
		Plugins:       testPluginRecords,
	}))
	return dir
}

// Plugin failures are warnings (exit 0), but an update in which no plugin got
// updated and nothing else ran did nothing and must not report SUCCESS.
func TestUpdateFunnelWhenPluginsFail(t *testing.T) {
	codexFailed := installer.PluginUpdate{Agent: "Codex CLI", Err: errCodexNotOnPath}
	cases := []struct {
		name    string
		skills  map[string]string
		results []installer.PluginUpdate
		migrate bool
		check   bool
		refErr  error
		want    string
	}{
		{name: "all plugins failed", results: []installer.PluginUpdate{codexFailed}, want: "FINISHED:FAILURE:CLI_NOT_ON_PATH"},
		{name: "one of two plugins failed", results: []installer.PluginUpdate{{Agent: "Claude Code", Version: "0.2.7"}, codexFailed}, want: "FINISHED:SUCCESS:"},
		{name: "plugin failed, file skills reconciled", skills: map[string]string{"databricks": "0.2.6"}, results: []installer.PluginUpdate{codexFailed}, want: "FINISHED:SUCCESS:"},
		{name: "plugin failed, raw skills migrated", results: []installer.PluginUpdate{codexFailed}, migrate: true, want: "FINISHED:SUCCESS:"},
		{name: "skills ref lookup failed", refErr: fmt.Errorf("resolve latest skills release: %w", context.DeadlineExceeded), want: "FINISHED:FAILURE:TIMEOUT"},
		{name: "check, plugins checked", check: true, want: "FINISHED:SUCCESS:"},
		{name: "check, skills ref lookup failed", check: true, refErr: fmt.Errorf("resolve latest skills release: %w", context.DeadlineExceeded), want: "FINISHED:FAILURE:TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCommandEnv(t)
			dir := saveGlobalState(t, tc.skills)
			resetUpdatePluginSeams(t)
			origRef, origUpdateSkills := updateGetSkillsRefFn, updateSkillsFn
			t.Cleanup(func() { updateGetSkillsRefFn, updateSkillsFn = origRef, origUpdateSkills })

			updateGetSkillsRefFn = func(context.Context) (string, bool, error) {
				return "v0.2.7", true, tc.refErr
			}
			updatePluginsFn = func(context.Context, string, string) ([]installer.PluginUpdate, error) {
				assert.False(t, tc.check, "--check must not update plugins")
				return tc.results, nil
			}
			reconciled := 0
			updateSkillsFn = func(context.Context, installer.ManifestSource, []*agents.Agent, installer.UpdateOptions) (*installer.UpdateResult, error) {
				reconciled++
				return &installer.UpdateResult{}, nil
			}
			// Migrate Copilot's raw skills to a plugin, leaving no file skills
			// behind, so only the migration counts as work done.
			hasManagedRawSkillsForAgentFn = func(_ context.Context, a *agents.Agent, _ string) (bool, error) {
				return tc.migrate && a.Name == agents.NameCopilot, nil
			}
			updateInstallPluginForAgentFn = func(context.Context, *agents.Agent, string, string) (installer.PluginRecord, error) {
				return installer.PluginRecord{Plugin: "databricks", Version: "0.2.7"}, nil
			}
			updateRecordPluginInstallsFn = func(_ context.Context, _ string, records map[string]installer.PluginRecord, _ string) error {
				plugins := maps.Clone(testPluginRecords)
				maps.Copy(plugins, records)
				return installer.SaveState(dir, &installer.InstallState{SchemaVersion: 2, Release: "v0.2.7", Plugins: plugins})
			}
			updateCleanupLegacyFn = func(context.Context, *agents.Agent, string) error { return nil }

			ctx := telemetry.WithNewLogger(cmdio.MockDiscard(t.Context()))
			cmd := NewUpdateCmd()
			cmd.SetContext(ctx)
			args := []string{"--scope=global"}
			if tc.check {
				args = append(args, "--check")
			}
			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute())
			assert.Equal(t, []string{"STARTED", tc.want}, funnelShape(funnelEvents(ctx)))
			if tc.skills == nil {
				assert.Zero(t, reconciled, "an install without file skills has nothing to reconcile")
			}
		})
	}
}

// Plugin and skill removal failures are warnings (exit 0), but an uninstall
// that removed nothing because every removal failed must not report SUCCESS.
func TestUninstallFunnelWhenRemovalsFail(t *testing.T) {
	skillFailed := fmt.Errorf("remove skill: %w", fs.ErrPermission)
	cases := []struct {
		name   string
		result installer.UninstallResult
		want   string
	}{
		{name: "all plugins failed", result: installer.UninstallResult{PluginFailures: []error{errCodexNotOnPath}}, want: "FINISHED:FAILURE:CLI_NOT_ON_PATH"},
		{name: "one of two plugins failed", result: installer.UninstallResult{PluginsRemoved: 1, PluginFailures: []error{errCodexNotOnPath}}, want: "FINISHED:SUCCESS:"},
		{name: "plugin failed, skills removed", result: installer.UninstallResult{SkillsRemoved: 1, PluginFailures: []error{errCodexNotOnPath}}, want: "FINISHED:SUCCESS:"},
		{name: "all skill removals failed", result: installer.UninstallResult{SkillFailures: []error{skillFailed}}, want: "FINISHED:FAILURE:PERMISSION_DENIED"},
		{name: "skill removal failed, plugin removed", result: installer.UninstallResult{PluginsRemoved: 1, SkillFailures: []error{skillFailed}}, want: "FINISHED:SUCCESS:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCommandEnv(t)
			saveGlobalState(t, nil)
			orig := uninstallSkillsFn
			t.Cleanup(func() { uninstallSkillsFn = orig })
			uninstallSkillsFn = func(context.Context, installer.UninstallOptions) (*installer.UninstallResult, error) {
				return &tc.result, nil
			}

			ctx := telemetry.WithNewLogger(cmdio.MockDiscard(t.Context()))
			cmd := NewUninstallCmd()
			cmd.SetContext(ctx)
			cmd.SetArgs([]string{"--scope=global"})
			require.NoError(t, cmd.Execute())
			assert.Equal(t, []string{"STARTED", tc.want}, funnelShape(funnelEvents(ctx)))
		})
	}
}
