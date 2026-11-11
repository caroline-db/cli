package installer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/databricks/cli/libs/aitools/agents"
	"github.com/databricks/cli/libs/log"
	"github.com/databricks/cli/libs/process"
)

// lookPath resolves a binary on PATH. It is a package-level var so tests can
// inject a fake resolver without depending on the host PATH.
var lookPath = exec.LookPath

const (
	// pluginProbeTimeout bounds the `<agent> plugin --help` capability check.
	// Agent CLIs are large Node/Rust binaries whose cold start (first run,
	// antivirus scan, slow container disk) can take several seconds, and a probe
	// that times out reports the agent as unavailable, so leave headroom.
	// It also bounds `<agent> plugin marketplace list` in marketplaceRegistered,
	// where a timeout means the marketplace is assumed already registered, so
	// install does not record it and uninstall leaves it registered.
	pluginProbeTimeout = 20 * time.Second
	// pluginCmdTimeout bounds an install/update/uninstall command, which may
	// clone the marketplace repo, so it gets more headroom than the probe.
	pluginCmdTimeout = 60 * time.Second
)

// BlockedError reports that a plugin operation could not be performed for an
// agent. The command layer maps Reason to a user-facing message and decides
// whether to skip-with-warning or hard-fail (per the non-TTY policy). It never
// causes a silent fall back to skills.
type BlockedError struct {
	Agent  string
	Reason string
	Detail string
	// TimedOut is set when the agent CLI was killed for exceeding its timeout,
	// so the failure can be told apart from a missing or broken CLI.
	TimedOut bool
}

// Reasons a plugin operation can be blocked.
const (
	// ReasonCLINotOnPath: the agent's CLI binary is not on PATH, or its CLI does
	// not expose a working `plugin` subcommand.
	ReasonCLINotOnPath = "cli-not-on-path"
	// ReasonInstallFailed: the agent's plugin CLI ran but returned an error.
	ReasonInstallFailed = "install-failed"
	// ReasonNoPlugin: the agent has no installable plugin. Callers filter these
	// out; it is guarded here to avoid a nil dereference.
	ReasonNoPlugin = "no-plugin"
)

func (e *BlockedError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s: %s", e.Agent, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Agent, e.Reason)
}

// runAgentCmd runs an agent CLI command with a timeout, returning stdout and any
// error. Errors are *process.ProcessError, which carries the captured stderr.
func runAgentCmd(ctx context.Context, timeout time.Duration, argv []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := process.Background(cctx, argv)
	if err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return out, &agentCmdTimeoutError{timeout: timeout, err: err}
	}
	return out, err
}

// agentCmdTimeoutError reports an agent CLI command killed by its timeout.
type agentCmdTimeoutError struct {
	timeout time.Duration
	err     error
}

func (e *agentCmdTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s", e.timeout)
}

func (e *agentCmdTimeoutError) Unwrap() error { return e.err }

// newBlockedError builds the BlockedError for a failed agent CLI command.
func newBlockedError(agent *agents.Agent, reason string, err error) *BlockedError {
	_, timedOut := errors.AsType[*agentCmdTimeoutError](err)
	return &BlockedError{Agent: agent.Name, Reason: reason, Detail: stderrOf(err), TimedOut: timedOut}
}

// stderrOf returns the captured stderr of a failed agent command, falling back
// to the error's own message. Callers must not branch on this string.
func stderrOf(err error) string {
	// A killed process has partial or no stderr; the timeout is the real cause.
	if terr, ok := errors.AsType[*agentCmdTimeoutError](err); ok {
		return terr.Error()
	}
	if perr, ok := errors.AsType[*process.ProcessError](err); ok {
		if s := strings.TrimSpace(perr.Stderr); s != "" {
			return s
		}
	}
	return err.Error()
}

// resolveAgentBinary resolves the agent's CLI binary to an absolute path.
// It refuses a binary that resolves only relative to the current directory
// (exec.ErrDot), so a malicious ./claude is never executed.
func resolveAgentBinary(agent *agents.Agent) (string, error) {
	if agent.Binary == "" {
		return "", fmt.Errorf("%s has no CLI binary", agent.DisplayName)
	}
	path, err := lookPath(agent.Binary)
	if err != nil {
		return "", fmt.Errorf("could not resolve %s on PATH: %w", agent.Binary, err)
	}
	return path, nil
}

// installTarget is the `<plugin>@<marketplace>` argument the agent CLIs accept,
// e.g. "databricks@databricks-agent-skills".
func installTarget(spec *agents.PluginSpec) string {
	return spec.ID + "@" + spec.Marketplace
}

func recordedInstallTarget(agent *agents.Agent, rec PluginRecord) string {
	plugin := rec.Plugin
	if plugin == "" {
		plugin = agent.Plugin.ID
	}
	return plugin + "@" + recordedMarketplace(agent, rec)
}

// marketplaceAddArgs builds the `plugin marketplace add <source>` argv (sans binary).
func marketplaceAddArgs(spec *agents.PluginSpec) []string {
	return []string{"plugin", "marketplace", "add", spec.Source}
}

// marketplaceRegistered reports whether the named marketplace is already listed
// by the agent's plugin CLI. On any uncertainty (command unsupported, error) it
// returns true, so we never claim to have added a marketplace we didn't, which
// keeps uninstall from de-registering a marketplace another plugin may share.
// The `plugin marketplace list` output shape is pending per-agent verification;
// until then the conservative default applies.
func marketplaceRegistered(ctx context.Context, bin, marketplace string) bool {
	out, err := runAgentCmd(ctx, pluginProbeTimeout, []string{bin, "plugin", "marketplace", "list"})
	if err != nil {
		return true
	}
	return strings.Contains(out, marketplace)
}

// marketplaceRemoveArgs builds the `plugin marketplace remove <name>` argv (sans binary).
func marketplaceRemoveArgs(spec *agents.PluginSpec) []string {
	return []string{"plugin", "marketplace", "remove", spec.Marketplace}
}

func marketplaceRemoveArgsForRecord(agent *agents.Agent, rec PluginRecord) []string {
	return []string{"plugin", "marketplace", "remove", recordedMarketplace(agent, rec)}
}

// recordedMarketplace is the marketplace a recorded install came from, or the
// agent's current one for records written before it was stored.
func recordedMarketplace(agent *agents.Agent, rec PluginRecord) string {
	if rec.Marketplace != "" {
		return rec.Marketplace
	}
	return agent.Plugin.Marketplace
}

// marketplaceUpdateArgs builds the marketplace refresh command for agents whose
// plugin CLI supports it. Claude's official marketplace can be stale locally,
// causing install/update to report that a published plugin does not exist.
// The refresh names the marketplace: without a name Claude refreshes every
// marketplace the user has added, and one unreachable third-party marketplace
// then fails the databricks install.
func marketplaceUpdateArgs(agent *agents.Agent, marketplace string) []string {
	if agent.Name == agents.NameClaudeCode {
		return []string{"plugin", "marketplace", "update", marketplace}
	}
	return nil
}

// pluginInstallArgs builds the per-agent install argv (sans binary). Codex uses
// `plugin add`; Claude is the only agent that accepts `--scope`.
func pluginInstallArgs(agent *agents.Agent, scope string) []string {
	target := installTarget(agent.Plugin)
	switch agent.Name {
	case agents.NameCodex:
		return []string{"plugin", "add", target}
	case agents.NameClaudeCode:
		args := []string{"plugin", "install", target}
		if scope != "" {
			args = append(args, "--scope", scope)
		}
		return args
	default:
		return []string{"plugin", "install", target}
	}
}

// pluginUpdateSteps builds the ordered per-agent update argv sets (sans binary).
// Codex updates in two steps: refresh the marketplace, then re-add. Both agents
// name the marketplace in the refresh, so an unreachable marketplace the user
// added for another plugin does not fail the update.
func pluginUpdateSteps(agent *agents.Agent, rec PluginRecord) [][]string {
	target := recordedInstallTarget(agent, rec)
	switch agent.Name {
	case agents.NameCodex:
		return [][]string{
			{"plugin", "marketplace", "upgrade", recordedMarketplace(agent, rec)},
			{"plugin", "add", target},
		}
	case agents.NameClaudeCode:
		return [][]string{
			marketplaceUpdateArgs(agent, recordedMarketplace(agent, rec)),
			appendScopeArg([]string{"plugin", "update", target}, rec.Scope),
		}
	default:
		return [][]string{{"plugin", "update", target}}
	}
}

// pluginUninstallArgs builds the per-agent uninstall argv (sans binary).
// Codex removes with `plugin remove`; the others use `plugin uninstall`.
func pluginUninstallArgs(agent *agents.Agent, rec PluginRecord) []string {
	target := recordedInstallTarget(agent, rec)
	switch agent.Name {
	case agents.NameCodex:
		return []string{"plugin", "remove", target}
	case agents.NameClaudeCode:
		return appendScopeArg([]string{"plugin", "uninstall", target}, rec.Scope)
	default:
		return []string{"plugin", "uninstall", target}
	}
}

func pluginDisableArgs(agent *agents.Agent, rec PluginRecord) []string {
	if agent.Name != agents.NameClaudeCode {
		return nil
	}
	return appendScopeArg([]string{"plugin", "disable", recordedInstallTarget(agent, rec)}, rec.Scope)
}

func appendScopeArg(args []string, scope string) []string {
	if scope == "" {
		return args
	}
	return append(args, "--scope", scope)
}

// probePluginCLI resolves the agent's binary and confirms its CLI exposes the
// plugin subcommand, so we don't register a marketplace on a CLI that can't
// install plugins. Returns the resolved absolute path.
func probePluginCLI(ctx context.Context, agent *agents.Agent) (string, error) {
	bin, err := resolveAgentBinary(agent)
	if err != nil {
		return "", &BlockedError{Agent: agent.Name, Reason: ReasonCLINotOnPath, Detail: err.Error()}
	}
	if _, err := runAgentCmd(ctx, pluginProbeTimeout, []string{bin, "plugin", "--help"}); err != nil && !windowsShimPrintedHelp(bin, err) {
		return "", newBlockedError(agent, ReasonCLINotOnPath, err)
	}
	return bin, nil
}

// probeGOOS is runtime.GOOS; tests replace it to exercise the Windows path.
var probeGOOS = runtime.GOOS

// pluginHelpUsage and pluginHelpCommand match the help that `<agent> plugin
// --help` prints: a "Usage: <bin> plugin" line and an indented command list
// that includes install and marketplace. An error message that merely
// mentions those words has neither shape.
var (
	pluginHelpUsage   = regexp.MustCompile(`(?im)^usage:[ \t]+\S+[ \t]+plugin\b`)
	pluginHelpInstall = regexp.MustCompile(`(?m)^[ \t]+(install|add)\b`)
	pluginHelpMarket  = regexp.MustCompile(`(?m)^[ \t]+marketplace\b`)
)

// windowsShimPrintedHelp reports whether a failed probe is the Windows shim
// quirk: codex.cmd and copilot.bat can print the full `plugin --help` and then
// exit 1, which marked a working CLI as missing. It is deliberately narrow: Windows only, a
// .cmd/.bat shim only, never a timeout, and only when the output is the help
// itself, so a real failure on any platform still blocks the install.
func windowsShimPrintedHelp(bin string, err error) bool {
	if probeGOOS != "windows" {
		return false
	}
	if ext := strings.ToLower(filepath.Ext(bin)); ext != ".cmd" && ext != ".bat" {
		return false
	}
	if _, ok := errors.AsType[*agentCmdTimeoutError](err); ok {
		return false
	}
	perr, ok := errors.AsType[*process.ProcessError](err)
	if !ok {
		return false
	}
	out := perr.Stdout + "\n" + perr.Stderr
	return pluginHelpUsage.MatchString(out) && pluginHelpInstall.MatchString(out) && pluginHelpMarket.MatchString(out)
}

// InstallPluginForAgent registers the databricks marketplace and installs the
// plugin through the agent's own CLI, returning the record to persist in state.
// It never falls back to skills: a blocked install returns a *BlockedError.
func InstallPluginForAgent(ctx context.Context, agent *agents.Agent, nativeScope, ref string) (PluginRecord, error) {
	if agent.Plugin == nil {
		return PluginRecord{}, &BlockedError{Agent: agent.Name, Reason: ReasonNoPlugin}
	}

	bin, err := probePluginCLI(ctx, agent)
	if err != nil {
		return PluginRecord{}, err
	}

	// Register the marketplace. We only record InstalledMarketplace (and thus
	// later de-register on uninstall) when the marketplace was absent before and
	// our add succeeded, so we never remove a marketplace another plugin shares.
	// On any uncertainty marketplaceRegistered returns true, keeping us off the
	// de-register path.
	installedMarketplace := false
	if agent.Plugin.Source != "" {
		alreadyPresent := marketplaceRegistered(ctx, bin, agent.Plugin.Marketplace)
		_, addErr := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, marketplaceAddArgs(agent.Plugin)))
		installedMarketplace = addErr == nil && !alreadyPresent
	}
	// A failed refresh is not fatal: the local copy of the marketplace usually
	// still lists the plugin, so try the install and report the refresh only if
	// that fails too. The local copy may be out of date, so warn and leave the
	// recorded version empty below.
	var refreshErr error
	if args := marketplaceUpdateArgs(agent, agent.Plugin.Marketplace); args != nil {
		if _, err := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, args)); err != nil {
			log.Warnf(ctx, "Could not refresh the %s marketplace, installing the %s plugin from the local copy; run `databricks aitools update` later: %s", agent.Plugin.Marketplace, agent.DisplayName, stderrOf(err))
			refreshErr = err
		}
	}

	if _, err := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, pluginInstallArgs(agent, nativeScope))); err != nil {
		// Roll back a marketplace we just added so a failed install doesn't
		// leave an orphaned, untracked marketplace registration behind.
		if installedMarketplace {
			if _, rmErr := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, marketplaceRemoveArgs(agent.Plugin))); rmErr != nil {
				log.Warnf(ctx, "%s plugin install failed and the marketplace could not be de-registered: %v", agent.DisplayName, rmErr)
			}
		}
		blocked := newBlockedError(agent, ReasonInstallFailed, err)
		if refreshErr != nil {
			blocked.Detail += " (refreshing the marketplace also failed: " + stderrOf(refreshErr) + ")"
		}
		return PluginRecord{}, blocked
	}

	// After a failed refresh we can't tell which version the local copy held,
	// so the record says "version unknown" until the next successful update.
	version := DisplaySkillsVersion(ref)
	if refreshErr != nil {
		version = ""
	}
	return PluginRecord{
		Marketplace:          agent.Plugin.Marketplace,
		Plugin:               agent.Plugin.ID,
		Scope:                nativeScope,
		Version:              version,
		InstalledMarketplace: installedMarketplace,
	}, nil
}

// UpdatePluginForAgent updates the plugin through the agent's own CLI. The
// plugin's own update handles content the release dropped, so there is no
// per-skill prune for plugin agents.
func UpdatePluginForAgent(ctx context.Context, agent *agents.Agent, rec PluginRecord) error {
	if agent.Plugin == nil {
		return &BlockedError{Agent: agent.Name, Reason: ReasonNoPlugin}
	}
	bin, err := resolveAgentBinary(agent)
	if err != nil {
		return &BlockedError{Agent: agent.Name, Reason: ReasonCLINotOnPath, Detail: err.Error()}
	}
	for _, args := range pluginUpdateSteps(agent, rec) {
		if _, err := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, args)); err != nil {
			return newBlockedError(agent, ReasonInstallFailed, err)
		}
	}
	return nil
}

// UninstallPluginForAgent removes the plugin through the agent's own CLI, and
// de-registers the marketplace only when this CLI registered it and the caller
// did not ask to keep it. It never removes a marketplace another plugin shares.
//
// A returned error means the plugin itself could not be removed, so the caller
// should keep its state record. Once the plugin is removed, a failure to
// de-register the marketplace is only warned about (not returned): the plugin is
// gone, so the record is cleared, and the leftover marketplace registration is
// harmless and can be removed manually.
func UninstallPluginForAgent(ctx context.Context, agent *agents.Agent, rec PluginRecord, keepMarketplace bool) error {
	if agent.Plugin == nil {
		return &BlockedError{Agent: agent.Name, Reason: ReasonNoPlugin}
	}
	bin, err := resolveAgentBinary(agent)
	if err != nil {
		return &BlockedError{Agent: agent.Name, Reason: ReasonCLINotOnPath, Detail: err.Error()}
	}
	if args := pluginDisableArgs(agent, rec); args != nil {
		if _, err := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, args)); err != nil && !claudeAlreadyDisabled(err) {
			return newBlockedError(agent, ReasonInstallFailed, err)
		}
	}
	if _, err := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, pluginUninstallArgs(agent, rec))); err != nil {
		return newBlockedError(agent, ReasonInstallFailed, err)
	}
	// Never de-register a Shared marketplace, it is shared infrastructure we don't own
	if rec.InstalledMarketplace && !keepMarketplace && !agent.Plugin.Shared {
		if _, err := runAgentCmd(ctx, pluginCmdTimeout, prepend(bin, marketplaceRemoveArgsForRecord(agent, rec))); err != nil {
			log.Warnf(ctx, "Removed the %s plugin but could not de-register its marketplace (remove it manually if needed): %v", agent.DisplayName, stderrOf(err))
		}
	}
	return nil
}

func claudeAlreadyDisabled(err error) bool {
	return strings.Contains(strings.ToLower(stderrOf(err)), "already disabled")
}

// RecordPluginInstalls persists plugin install records into the state file for
// the given CLI scope (global or project), creating state if none exists. ref
// is the resolved skills release the install corresponds to.
func RecordPluginInstalls(ctx context.Context, cliScope string, records map[string]PluginRecord, ref string) error {
	dir, err := skillsDir(ctx, cliScope)
	if err != nil {
		return err
	}
	state, err := LoadState(dir)
	if err != nil {
		return err
	}
	if state == nil {
		// Initialize all maps so a later skills install/update can assign into a
		// plugin-only state without hitting a nil map.
		state = &InstallState{
			SchemaVersion: schemaVersionV2,
			Skills:        map[string]string{},
			RepoDirs:      map[string]string{},
			Files:         map[string]FileRecord{},
		}
	}
	if state.Plugins == nil {
		state.Plugins = make(map[string]PluginRecord, len(records))
	}
	maps.Copy(state.Plugins, records)
	state.Release = ref
	state.LastUpdated = time.Now()
	state.Scope = cliScope
	return SaveState(dir, state)
}

// prepend returns a fresh argv with bin as argv[0] followed by args.
func prepend(bin string, args []string) []string {
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, bin)
	return append(argv, args...)
}
