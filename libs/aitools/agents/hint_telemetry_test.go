package agents

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/databricks/cli/internal/testutil"
	"github.com/databricks/cli/libs/cmdio"
	"github.com/databricks/cli/libs/env"
	"github.com/databricks/cli/libs/telemetry"
	"github.com/databricks/cli/libs/telemetry/protos"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hintEvents returns the hint-shown events buffered on ctx, failing on any
// other event kind so a stray event cannot hide behind the filter.
func hintEvents(t *testing.T, ctx context.Context) []protos.AitoolsHintEvent {
	var out []protos.AitoolsHintEvent
	for _, e := range telemetry.BufferedEvents(ctx) {
		require.NotNil(t, e.AitoolsHintEvent, "unexpected non-hint event: %+v", e)
		out = append(out, *e.AitoolsHintEvent)
	}
	return out
}

func TestMaybeHint_PrintedLogsOneEvent(t *testing.T) {
	setClaudeAgent(t)
	ctx, stderr := claudeCtx(t, t.TempDir())
	ctx = telemetry.WithNewLogger(ctx)

	MaybeHint(ctx, regularCmd())

	require.Contains(t, stderr.String(), hintText)
	assert.Equal(t, []protos.AitoolsHintEvent{{HintKind: protos.AitoolsHintKindInstallText}}, hintEvents(t, ctx))
}

func TestMaybeHint_NotPrintedLogsNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string) context.Context
		cmd   func() *cobra.Command
	}{
		{
			name: "human caller",
			setup: func(t *testing.T, home string) context.Context {
				testutil.SetAgentEnv(t)
				ctx, _ := claudeCtx(t, home)
				return ctx
			},
			cmd: regularCmd,
		},
		{
			name: "plugin installed",
			setup: func(t *testing.T, home string) context.Context {
				setClaudeAgent(t)
				writePluginManifest(t, home, `{"plugins":{"databricks@claude-plugins-official":[{"scope":"user"}]}}`)
				ctx, _ := claudeCtx(t, home)
				return ctx
			},
			cmd: regularCmd,
		},
		{
			name: "aitools command",
			setup: func(t *testing.T, home string) context.Context {
				setClaudeAgent(t)
				ctx, _ := claudeCtx(t, home)
				return ctx
			},
			cmd: func() *cobra.Command {
				parent := &cobra.Command{Use: aiToolsCommandName}
				child := &cobra.Command{Use: "install"}
				parent.AddCommand(child)
				return child
			},
		},
		{
			name: "no IO",
			setup: func(t *testing.T, home string) context.Context {
				setClaudeAgent(t)
				return env.WithUserHomeDir(t.Context(), home)
			},
			cmd: regularCmd,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := telemetry.WithNewLogger(tc.setup(t, t.TempDir()))
			MaybeHint(ctx, tc.cmd())
			assert.Empty(t, telemetry.BufferedEvents(ctx))
		})
	}
}

func TestMaybeHint_ThrottledSessionLogsOnce(t *testing.T) {
	setClaudeAgent(t)
	home := t.TempDir()
	cacheDir := t.TempDir()
	newCtx := func() context.Context {
		ctx, _ := cmdio.NewTestContextWithStderr(t.Context())
		ctx = env.WithUserHomeDir(ctx, home)
		ctx = env.Set(ctx, "DATABRICKS_CACHE_DIR", cacheDir)
		ctx = env.Set(ctx, "DATABRICKS_CACHE_ENABLED", "true")
		ctx = env.Set(ctx, "CLAUDE_CONFIG_DIR", "")
		ctx = env.Set(ctx, claudeSessionEnv, "session-abc")
		return telemetry.WithNewLogger(ctx)
	}

	first := newCtx()
	MaybeHint(first, regularCmd())
	assert.Len(t, hintEvents(t, first), 1)

	second := newCtx()
	MaybeHint(second, regularCmd())
	assert.Empty(t, telemetry.BufferedEvents(second))
}

func TestLogHintShown(t *testing.T) {
	ctx := telemetry.WithNewLogger(t.Context())
	LogHintShown(ctx, protos.AitoolsHintKindClaudeCodeNative)
	assert.Equal(t, []protos.AitoolsHintEvent{{HintKind: protos.AitoolsHintKindClaudeCodeNative}}, hintEvents(t, ctx))
}

// setupAgentWithoutSkills makes the registry a single detected agent with no
// Databricks skills, so RecommendSkillsInstall recommends installing them. It
// returns the temporary home.
func setupAgentWithoutSkills(t *testing.T) string {
	tmpDir := t.TempDir() // before SetAgentEnv clears TMPDIR / TEMP
	testutil.SetAgentEnv(t)
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".test-agent"), 0o755))

	origRegistry := Registry
	Registry = []*Agent{{
		Name:        "test-agent",
		DisplayName: "Test Agent",
		ConfigDir:   func(context.Context) (string, error) { return filepath.Join(tmpDir, ".test-agent"), nil },
	}}
	t.Cleanup(func() { Registry = origRegistry })
	return tmpDir
}

func TestRecommendSkillsInstallTipLogsHint(t *testing.T) {
	setupAgentWithoutSkills(t)

	ctx, _ := cmdio.NewTestContextWithStderr(t.Context())
	ctx = telemetry.WithNewLogger(ctx)
	require.NoError(t, RecommendSkillsInstall(ctx, noopInstall))
	assert.Equal(t, []protos.AitoolsHintEvent{{HintKind: protos.AitoolsHintKindAppsInitTip}}, hintEvents(t, ctx))
}

// The prompt is logged when shown, so a declined prompt still counts.
func TestRecommendSkillsInstallPromptLogsHint(t *testing.T) {
	setupAgentWithoutSkills(t)

	ctx, testIO := cmdio.SetupTest(t.Context(), cmdio.TestOptions{PromptSupported: true})
	defer testIO.Done()
	ctx = telemetry.WithNewLogger(ctx)
	go func() { _, _ = io.Copy(io.Discard, testIO.Stderr) }()

	errc := make(chan error, 1)
	go func() { errc <- RecommendSkillsInstall(ctx, noopInstall) }()
	_, err := testIO.Stdin.WriteString("n\n")
	require.NoError(t, err)
	require.NoError(t, testIO.Stdin.Flush())
	require.NoError(t, <-errc)

	assert.Equal(t, []protos.AitoolsHintEvent{{HintKind: protos.AitoolsHintKindAppsInitPrompt}}, hintEvents(t, ctx))
}

func TestRecommendSkillsInstallSkillsPresentLogsNothing(t *testing.T) {
	home := setupAgentWithoutSkills(t)
	writeCanonicalSkill(t, home, "databricks-core")

	ctx, _ := cmdio.NewTestContextWithStderr(t.Context())
	ctx = telemetry.WithNewLogger(ctx)
	require.NoError(t, RecommendSkillsInstall(ctx, noopInstall))
	assert.Empty(t, telemetry.BufferedEvents(ctx))
}
