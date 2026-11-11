package aitools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/huh"
	"github.com/databricks/cli/libs/aitools/agents"
	"github.com/databricks/cli/libs/aitools/installer"
	"github.com/databricks/cli/libs/clicompat"
	"github.com/databricks/cli/libs/env"
	"github.com/databricks/cli/libs/shellquote"
	"github.com/databricks/cli/libs/versioncheck"
)

const (
	skillsReleasesURL = "https://github.com/databricks/databricks-agent-skills/releases"
	cliIssuesURL      = "https://github.com/databricks/cli/issues"
)

// errNothingInstalled is returned when every agent in the plan was skipped or
// failed. Each agent's reason and recovery step is printed above it, so the
// message only has to make the exit code non-zero.
var errNothingInstalled = errors.New("nothing was installed; follow the steps above for each agent")

// nothingInstalledError is errNothingInstalled carrying the per-agent causes,
// so classifyInstallError reports the first agent's category instead of
// UNCATEGORIZED. Its message stays errNothingInstalled's.
type nothingInstalledError struct {
	causes []error
}

func (e *nothingInstalledError) Error() string { return errNothingInstalled.Error() }
func (e *nothingInstalledError) Unwrap() []error {
	return append([]error{errNothingInstalled}, e.causes...)
}

// newNothingInstalledError collects the failed and skipped outcomes as causes,
// tagged with the category each one already carries.
func newNothingInstalledError(outcomes []agentOutcome) error {
	var causes []error
	for _, o := range outcomes {
		if o.status != outcomeInstalled {
			causes = append(causes, withCategory(errors.New(o.message), o.errorCategory))
		}
	}
	return &nothingInstalledError{causes: causes}
}

type rerunInvocationKey struct{}

// withRerunInvocation records the flags a recovery step should tell the user
// to re-run with: this run's own choices, so the retry does the same thing.
func withRerunInvocation(ctx context.Context, inv installInvocation) context.Context {
	return context.WithValue(ctx, rerunInvocationKey{}, inv)
}

// rerunInvocation returns the flags recorded by withRerunInvocation, or none
// (the bare install command) before any choice was made.
func rerunInvocation(ctx context.Context) installInvocation {
	inv, _ := ctx.Value(rerunInvocationKey{}).(installInvocation)
	return inv
}

// rerunCommand names the command a recovery step tells the user to re-run:
// the command for the flags recorded in ctx plus extra flags, in quotes. When
// a flag value cannot be printed as part of a command that pastes safely, it
// asks the user to repeat their own command instead.
func rerunCommand(ctx context.Context, extra ...string) string {
	return rerunPhrase(runtime.GOOS, rerunInvocation(ctx), extra...)
}

func rerunPhrase(goos string, inv installInvocation, extra ...string) string {
	cmd, ok := renderInstallCommand(goos, inv)
	if !ok {
		phrase := "your original install command"
		if len(extra) > 0 {
			phrase += " with " + strings.Join(extra, " ")
		}
		return phrase
	}
	return "'" + strings.Join(append([]string{cmd}, extra...), " ") + "'"
}

// installInvocation is the subset of install flags a re-run must repeat.
// agents holds resolved registry names, which need no quoting; path and
// skills are user input and are quoted for the shell.
type installInvocation struct {
	path, scope, agents, skills string
	skillsOnly, experimental    bool
}

// renderInstallCommand renders inv as a command line for the shell a user on
// goos pastes it into. It reports false when a value cannot be quoted so that
// the shell passes it on unchanged.
func renderInstallCommand(goos string, inv installInvocation) (string, bool) {
	args := []string{"databricks aitools install"}
	if inv.path != "" {
		path, ok := quoteArg(goos, inv.path)
		if !ok {
			return "", false
		}
		args = append(args, "--path="+path)
	}
	if inv.scope != "" {
		args = append(args, "--scope="+inv.scope)
	}
	if inv.agents != "" {
		args = append(args, "--agents="+inv.agents)
	}
	if inv.skillsOnly {
		args = append(args, "--skills-only")
	}
	if inv.skills != "" {
		// Normalized as install parses it, so "a, b" pastes as one value.
		skills, ok := quoteArg(goos, strings.Join(splitAndTrim(inv.skills), ","))
		if !ok {
			return "", false
		}
		args = append(args, "--skills="+skills)
	}
	if inv.experimental {
		args = append(args, "--experimental")
	}
	return strings.Join(args, " "), true
}

// quoteArg quotes a flag value for the shell a user on goos pastes the printed
// command into, only when it needs it, so the common case stays readable:
// cmd.exe or PowerShell on Windows, a POSIX shell elsewhere. It reports false
// for a value no quoting keeps literal there, and for any value with a control
// character: a line break in a pasted command runs what follows it.
func quoteArg(goos, value string) (string, bool) {
	if goos == "windows" {
		return shellquote.WindowsArg(value)
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return "", false
	}
	return shellquote.BashArg(value), true
}

// recoveryError appends a recovery step to an install failure. It unwraps to the
// original error so telemetry classification is unchanged.
type recoveryError struct {
	err      error
	recovery string
}

// Error appends the recovery step without its final period: an error string
// must not end with punctuation.
func (e *recoveryError) Error() string {
	return e.err.Error() + "\n" + strings.TrimSuffix(e.recovery, ".")
}
func (e *recoveryError) Unwrap() error { return e.err }

// displayError replaces the message of an error that is already explained by
// lines printed above it, keeping the cause for classification.
type displayError struct {
	msg string
	err error
}

func (e *displayError) Error() string { return e.msg }
func (e *displayError) Unwrap() error { return e.err }

// withRecovery attaches the recovery step for err, if it has none yet.
func withRecovery(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*recoveryError](err); ok {
		return err
	}
	step := installRecovery(ctx, err)
	if step == "" {
		return err
	}
	return &recoveryError{err: err, recovery: step}
}

// installRecovery returns the next step a user can take after a failed install,
// or "" when the error already says what to do (flag misuse, per-agent failures
// printed with their own steps).
func installRecovery(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if _, ok := errors.AsType[*agentErrors](err); ok {
		return ""
	}
	if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, errNoAgentsSelected) {
		return "To install without prompts, name the scope and agents, for example: databricks aitools install --scope=global --agents=" + agents.NameClaudeCode
	}
	if ferr, ok := errors.AsType[*installer.FetchError](err); ok {
		return fetchRecovery(ctx, ferr)
	}
	if serr, ok := errors.AsType[*installer.StateError](err); ok {
		return stateRecovery(ctx, serr)
	}
	if skerr, ok := errors.AsType[*installer.SkillError](err); ok {
		return skillRecovery(ctx, skerr)
	}
	if perr, ok := errors.AsType[*fs.PathError](err); ok && errors.Is(err, fs.ErrPermission) {
		return fmt.Sprintf("Check that you own %s and can write to it (an earlier run with sudo can leave it owned by root), then re-run %s.", perr.Path, rerunCommand(ctx))
	}
	return "Re-run " + rerunCommand(ctx, "--debug") + " to see details. If it keeps failing, report it at " + cliIssuesURL + "."
}

// stateRecovery is the step for a state file that cannot be parsed. Without
// state, an install of named skills is refused while other skills are on disk
// (they would stay untracked), so the re-run installs all skills, which
// rebuilds complete state.
func stateRecovery(ctx context.Context, serr *installer.StateError) string {
	inv := rerunInvocation(ctx)
	step := fmt.Sprintf("Move the install state file %s aside (for example, rename it to %s.bak), then re-run ", serr.Path, filepath.Base(serr.Path))
	if inv.skills == "" {
		return step + rerunPhrase(runtime.GOOS, inv) + "."
	}
	// Without --skills the command holds no user-typed value, so it is always
	// printed as a command.
	inv.skills = ""
	return step + rerunPhrase(runtime.GOOS, inv) + " to reinstall all skills and rebuild the state file. An install of named skills needs the state file."
}

func fetchRecovery(ctx context.Context, ferr *installer.FetchError) string {
	rerun := rerunCommand(ctx)
	if ferr.Stage == installer.FetchStageRelease {
		cause := "Could not look up the latest Databricks skills release on api.github.com."
		if ferr.RateLimited {
			cause = "GitHub rate-limited the lookup of the latest Databricks skills release."
		}
		return cause + " Wait a few minutes and re-run " + rerun + ", or skip the lookup by setting DATABRICKS_SKILLS_REF to a release tag from " + skillsReleasesURL + "."
	}
	if errors.Is(ferr, clicompat.ErrNotFound) {
		if env.Get(ctx, "DATABRICKS_SKILLS_REF") != "" {
			return "No skills release exists at the version DATABRICKS_SKILLS_REF names. Unset it, or set it to a release tag from " + skillsReleasesURL + ", then re-run " + rerun + "."
		}
		return "The skills release this CLI version is pinned to is missing. Set DATABRICKS_SKILLS_REF to a release tag from " + skillsReleasesURL + " and re-run " + rerun + ", and report it at " + cliIssuesURL + "."
	}
	host := "raw.githubusercontent.com"
	if u, err := url.Parse(installer.GetSkillsBaseURL(ctx)); err == nil && u.Host != "" {
		host = u.Host
	}
	switch {
	case isManifestDecodeError(ferr):
		return host + " answered with something that is not a skills manifest (a proxy or captive portal page, or a broken mirror). Check what this machine gets from " + host + ", then re-run " + rerun + "."
	case ferr.RateLimited:
		return host + " rate-limited the download (HTTP " + strconv.Itoa(ferr.StatusCode) + "). Wait a few minutes and re-run " + rerun + "."
	case ferr.StatusCode >= 500:
		return host + " returned HTTP " + strconv.Itoa(ferr.StatusCode) + ", a server-side error. Wait a few minutes and re-run " + rerun + "."
	case ferr.StatusCode == http.StatusForbidden || ferr.StatusCode == http.StatusProxyAuthRequired:
		return host + " refused the download (HTTP " + strconv.Itoa(ferr.StatusCode) + "). A proxy or access policy may block it; ask your network admin to allow " + host + ", then re-run " + rerun + "."
	case ferr.StatusCode != 0:
		return host + " returned HTTP " + strconv.Itoa(ferr.StatusCode) + ". Re-run " + rerunCommand(ctx, "--debug") + " to see the URL, and report it at " + cliIssuesURL + "."
	default:
		return "Could not reach " + host + " to download Databricks skills. Check that this machine can reach it (set HTTPS_PROXY if you use a proxy), then re-run " + rerun + "."
	}
}

func skillRecovery(ctx context.Context, e *installer.SkillError) string {
	switch e.Reason {
	case installer.ReasonSkillNotFound:
		return "Run 'databricks aitools list' to see the available skills."
	case installer.ReasonVersionIncompatible:
		if _, cmd := versioncheck.DetectInstallMethod(ctx); cmd != "" {
			return "Upgrade the Databricks CLI with '" + cmd + "', then re-run the install."
		}
		return "Upgrade the Databricks CLI (see https://docs.databricks.com/dev-tools/cli/install), then re-run the install."
	default:
		// The experimental-skill message already names --experimental.
		return ""
	}
}

// installCommand is the install command for one agent in the plan's scope. The
// scope is always spelled out: without it a non-interactive run defaults to
// global, which is not what a project-scoped run asked for.
func installCommand(it agentPlanItem, extra ...string) string {
	args := append([]string{"databricks aitools install", "--agents=" + it.agent.Name, "--scope=" + it.cliScope}, extra...)
	return strings.Join(args, " ")
}

// skillsOnlyCommand is the raw-skills install for one agent, the escape hatch
// when its plugin cannot be installed.
func skillsOnlyCommand(it agentPlanItem) string {
	return installCommand(it, "--skills-only")
}

// pluginFailureText returns the one-line reason and the recovery step for a
// plugin agent that could not be set up.
func pluginFailureText(ctx context.Context, it agentPlanItem, err error) (reason, recovery string) {
	a := it.agent
	blocked, ok := errors.AsType[*installer.BlockedError](err)
	if !ok {
		return err.Error(), "Re-run '" + installCommand(it, "--debug") + "' to see details, or install skill files instead: " + skillsOnlyCommand(it)
	}
	switch {
	case blocked.TimedOut:
		return fmt.Sprintf("'%s plugin' %s", a.Binary, blocked.Detail),
			"Re-run '" + installCommand(it) + "' (an agent CLI's first start can be slow), or install skill files instead: " + skillsOnlyCommand(it)
	case blocked.Reason == installer.ReasonCLINotOnPath:
		reason := fmt.Sprintf("'%s' is not on PATH", a.Binary)
		if a.HasBinary(ctx) {
			reason = fmt.Sprintf("'%s plugin' is not available (%s)", a.Binary, blocked.Detail)
		}
		return reason, fmt.Sprintf("Install or update the %s CLI so '%s' is on PATH, then re-run '%s'. Or install skill files instead: %s", a.DisplayName, a.Binary, installCommand(it), skillsOnlyCommand(it))
	case blocked.Reason == installer.ReasonInstallFailed:
		manual := installer.ManualPluginInstallCommands(a, it.scope)
		return fmt.Sprintf("'%s plugin' failed: %s", a.Binary, blocked.Detail),
			"Run '" + strings.Join(manual, "', then '") + "' to see the full error, or install skill files instead: " + skillsOnlyCommand(it)
	default:
		return err.Error(), "Install skill files instead: " + skillsOnlyCommand(it)
	}
}

// skipRecovery returns the recovery step for an agent the plan skipped.
func skipRecovery(it agentPlanItem) string {
	return "Re-run with --scope=global to set up " + it.agent.DisplayName + "."
}

// fallsBackToSkills reports whether a failed plugin install should install raw
// skill files for the agent instead. That happens only when the agent's CLI
// cannot be run (a desktop or IDE-only install has the config dir but no CLI
// on PATH) and the user did not name the agent: skills are what that agent can
// use today, and a later run with the CLI on PATH switches it to the plugin.
// A named agent keeps failing loudly, so --agents never changes delivery.
func fallsBackToSkills(it agentPlanItem, err error) bool {
	if it.explicit {
		return false
	}
	blocked, ok := errors.AsType[*installer.BlockedError](err)
	return ok && blocked.Reason == installer.ReasonCLINotOnPath && !blocked.TimedOut
}

// skillsFallbackText is the notice printed when an agent gets skill files
// instead of the plugin.
func skillsFallbackText(ctx context.Context, it agentPlanItem, err error) string {
	reason, _ := pluginFailureText(ctx, it, err)
	return fmt.Sprintf("Skipped the plugin for %s: %s. Installing skill files instead; to switch to the plugin, make '%s plugin' work and run '%s'.",
		it.agent.DisplayName, reason, it.agent.Binary, installCommand(it))
}

// pluginAlreadyInstalled reports whether the agent already has the databricks
// plugin for this install scope. When the CLI can read the agent's own plugin
// manifest, the manifest decides, as in agents.DatabricksTooling; otherwise the
// CLI's install state for the scope does. A global install serves every
// project, so only a machine-wide plugin covers it; a project or local plugin
// counts only for a project-scope install inside its own project.
func pluginAlreadyInstalled(ctx context.Context, a *agents.Agent, scope string) bool {
	if a.HasPluginManifest() {
		project := ""
		if scope == installer.ScopeProject {
			if cwd, err := os.Getwd(); err == nil {
				project = cwd
			}
		}
		return a.DatabricksPluginActiveIn(ctx, project)
	}
	dir, err := installer.GlobalSkillsDir(ctx)
	if scope == installer.ScopeProject {
		dir, err = installer.ProjectSkillsDir(ctx)
	}
	if err != nil {
		return false
	}
	state, err := installer.LoadState(dir)
	if err != nil || state == nil {
		return false
	}
	_, ok := state.Plugins[a.Name]
	return ok
}

// pluginKeptText is the notice for an agent whose CLI can't run but which
// already has the plugin, so no skill files are added next to it.
func pluginKeptText(ctx context.Context, it agentPlanItem, err error) string {
	reason, _ := pluginFailureText(ctx, it, err)
	return fmt.Sprintf("%s already has the databricks plugin; it was not refreshed because %s. To refresh it, make '%s plugin' work and run '%s'.",
		it.agent.DisplayName, reason, it.agent.Binary, installCommand(it))
}
