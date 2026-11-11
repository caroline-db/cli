package aitools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/databricks/cli/libs/aitools/installer"
	"github.com/databricks/cli/libs/clicompat"
	"github.com/databricks/cli/libs/telemetry/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyInstallError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want protos.AitoolsErrorCategory
	}{
		{
			name: "nil is success",
			err:  nil,
			want: protos.AitoolsErrorCategoryUnspecified,
		},
		{
			name: "blocked cli not on path",
			err:  &installer.BlockedError{Agent: "claude-code", Reason: installer.ReasonCLINotOnPath},
			want: protos.AitoolsErrorCategoryCLINotOnPath,
		},
		{
			name: "blocked install failed",
			err:  &installer.BlockedError{Agent: "codex", Reason: installer.ReasonInstallFailed},
			want: protos.AitoolsErrorCategoryPluginInstallFailed,
		},
		{
			name: "blocked timeout wins over its reason",
			err:  &installer.BlockedError{Agent: "claude-code", Reason: installer.ReasonCLINotOnPath, TimedOut: true},
			want: protos.AitoolsErrorCategoryPluginCLITimeout,
		},
		{
			name: "release lookup failed",
			err:  fmt.Errorf("resolve latest skills release: %w", &installer.FetchError{Stage: installer.FetchStageRelease, Err: &net.DNSError{Err: "no such host"}}),
			want: protos.AitoolsErrorCategoryReleaseLookupFailed,
		},
		{
			name: "release payload that does not decode is not an invalid manifest",
			err:  &installer.FetchError{Stage: installer.FetchStageRelease, StatusCode: 200, Err: fmt.Errorf("parse latest skills release: %w", &json.SyntaxError{})},
			want: protos.AitoolsErrorCategoryReleaseLookupFailed,
		},
		{
			name: "release lookup rate limited",
			err:  &installer.FetchError{Stage: installer.FetchStageRelease, StatusCode: 403, RateLimited: true, Err: errors.New("HTTP 403")},
			want: protos.AitoolsErrorCategoryRateLimited,
		},
		{
			name: "release lookup denied without rate-limit evidence",
			err:  &installer.FetchError{Stage: installer.FetchStageRelease, StatusCode: 403, Err: errors.New("HTTP 403")},
			want: protos.AitoolsErrorCategoryReleaseLookupFailed,
		},
		{
			name: "manifest fetch rate limited",
			err:  &installer.FetchError{Stage: installer.FetchStageManifest, StatusCode: 429, RateLimited: true, Err: errors.New("HTTP 429")},
			want: protos.AitoolsErrorCategoryRateLimited,
		},
		{
			name: "manifest fetch network error is not plain NETWORK",
			err:  &installer.FetchError{Stage: installer.FetchStageManifest, Err: &net.DNSError{Err: "no such host"}},
			want: protos.AitoolsErrorCategorySkillsFetchFailed,
		},
		{
			name: "skill file fetch failed",
			err:  fmt.Errorf("failed to fetch skills/x/SKILL.md: %w", &installer.FetchError{Stage: installer.FetchStageFile, StatusCode: 500, Err: errors.New("HTTP 500")}),
			want: protos.AitoolsErrorCategorySkillsFetchFailed,
		},
		{
			name: "manifest missing at the pinned ref",
			err:  &installer.FetchError{Stage: installer.FetchStageManifest, StatusCode: 404, Err: fmt.Errorf("skills manifest at x@v9: %w", clicompat.ErrNotFound)},
			want: protos.AitoolsErrorCategoryManifestNotFound,
		},
		{
			name: "state file invalid",
			err:  fmt.Errorf("failed to load install state: %w", &installer.StateError{Path: "/s/.state.json", Err: errors.New("failed to parse state file")}),
			want: protos.AitoolsErrorCategoryStateInvalid,
		},
		{
			name: "blocked error with unknown reason is uncategorized",
			err:  &installer.BlockedError{Agent: "codex", Reason: "some-future-reason"},
			want: protos.AitoolsErrorCategoryUncategorized,
		},
		{
			name: "wrapped skill not found",
			err:  fmt.Errorf("resolve failed: %w", &installer.SkillError{Skill: "databricks", Reason: installer.ReasonSkillNotFound, Detail: "not found"}),
			want: protos.AitoolsErrorCategorySkillNotFound,
		},
		{
			name: "version incompatible",
			err:  &installer.SkillError{Skill: "databricks", Reason: installer.ReasonVersionIncompatible, Detail: "requires CLI version 0.5 (running 0.4)"},
			want: protos.AitoolsErrorCategoryVersionIncompatible,
		},
		{
			name: "experimental skill",
			err:  &installer.SkillError{Skill: "test-exp", Reason: installer.ReasonExperimentalSkill, Detail: "is experimental; use --experimental to install"},
			want: protos.AitoolsErrorCategoryExperimentalSkill,
		},
		{
			name: "skill error with unknown reason is uncategorized",
			err:  &installer.SkillError{Skill: "databricks", Reason: "some-future-reason"},
			want: protos.AitoolsErrorCategoryUncategorized,
		},
		{
			name: "blocked error joined with another error is still classified",
			err:  errors.Join(&installer.BlockedError{Agent: "codex", Reason: installer.ReasonInstallFailed}, errors.New("other")),
			want: protos.AitoolsErrorCategoryPluginInstallFailed,
		},
		{
			name: "unrecognized error is uncategorized",
			err:  errors.New("boom"),
			want: protos.AitoolsErrorCategoryUncategorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyInstallError(tc.err))
		})
	}
}

func TestClassifyInstallErrorGenericCauses(t *testing.T) {
	urlErr := &url.Error{Op: "Get", URL: "https://example.test", Err: errors.New("connection refused")}
	cases := []struct {
		name string
		err  error
		want protos.AitoolsErrorCategory
	}{
		{"network", fmt.Errorf("failed to fetch manifest: %w", urlErr), protos.AitoolsErrorCategoryNetwork},
		// The shape net/http returns for a refused connection: the Errno sits under the network types.
		{"network errno", fmt.Errorf("failed to fetch manifest: %w", &url.Error{Op: "Get", URL: "https://example.test", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}), protos.AitoolsErrorCategoryNetwork},
		// A sandbox that blocks outbound sockets: a network failure, even though the Errno is EACCES.
		{"network permission denied", fmt.Errorf("failed to fetch manifest: %w", &url.Error{Op: "Get", URL: "https://example.test", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EACCES)}}), protos.AitoolsErrorCategoryNetwork},
		{"network timeout", fmt.Errorf("failed to fetch manifest: %w", &url.Error{Op: "Get", URL: "https://example.test", Err: os.ErrDeadlineExceeded}), protos.AitoolsErrorCategoryTimeout},
		{"deadline", fmt.Errorf("fetch: %w", context.DeadlineExceeded), protos.AitoolsErrorCategoryTimeout},
		{"cancelled", fmt.Errorf("fetch: %w", context.Canceled), protos.AitoolsErrorCategoryCommandInterrupted},
		{"cancelled request", &url.Error{Op: "Get", URL: "https://example.test", Err: context.Canceled}, protos.AitoolsErrorCategoryCommandInterrupted},
		{"manifest missing", fmt.Errorf("skills manifest at x@y: %w", clicompat.ErrNotFound), protos.AitoolsErrorCategoryManifestNotFound},
		// The manifest, release payload and install state all decode JSON; only typed errors can tell them apart.
		{"json syntax error stays uncategorized", fmt.Errorf("failed to parse manifest: %w", json.Unmarshal([]byte("<html>"), &struct{}{})), protos.AitoolsErrorCategoryUncategorized},
		{"json type error stays uncategorized", fmt.Errorf("failed to parse manifest: %w", json.Unmarshal([]byte(`{"a":1}`), &struct{ A string }{})), protos.AitoolsErrorCategoryUncategorized},
		{"permission", fmt.Errorf("write: %w", &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrPermission}), protos.AitoolsErrorCategoryPermissionDenied},
		// syscall.Errno implements net.Error, so OS errors must not reach the network check.
		{"permission errno", fmt.Errorf("write: %w", &fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}), protos.AitoolsErrorCategoryPermissionDenied},
		{"permission on rename", fmt.Errorf("save state: %w", &os.LinkError{Op: "rename", Old: "/a", New: "/b", Err: syscall.EACCES}), protos.AitoolsErrorCategoryPermissionDenied},
		{"filesystem", fmt.Errorf("write: %w", &fs.PathError{Op: "mkdir", Path: "/x", Err: errors.New("read-only file system")}), protos.AitoolsErrorCategoryFilesystem},
		{"filesystem from os", fmt.Errorf("read state: %w", readMissingFile(t)), protos.AitoolsErrorCategoryFilesystem},
		{"filesystem on rename", fmt.Errorf("replace skill: %w", &os.LinkError{Op: "rename", Old: "/a", New: "/b", Err: syscall.EBUSY}), protos.AitoolsErrorCategoryFilesystem},
		{"filesystem syscall", fmt.Errorf("resolve cwd: %w", os.NewSyscallError("getwd", syscall.ENOENT)), protos.AitoolsErrorCategoryFilesystem},
		{"user abort", huh.ErrUserAborted, protos.AitoolsErrorCategoryUserAborted},
		{"tagged", withCategory(errors.New("bad flags"), protos.AitoolsErrorCategoryInvalidFlags), protos.AitoolsErrorCategoryInvalidFlags},
		{"prompt failure", promptError(errors.New("no tty")), protos.AitoolsErrorCategoryPromptFailed},
		{"prompt abort keeps abort", promptError(huh.ErrUserAborted), protos.AitoolsErrorCategoryUserAborted},
		{"typed error beats tag", withCategory(&installer.BlockedError{Reason: installer.ReasonCLINotOnPath}, protos.AitoolsErrorCategoryInvalidFlags), protos.AitoolsErrorCategoryCLINotOnPath},
		{"plain text stays uncategorized", errors.New("something"), protos.AitoolsErrorCategoryUncategorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyInstallError(tc.err))
		})
	}
}

func TestWithCategoryKeepsMessage(t *testing.T) {
	cause := errors.New("cannot use --skills-only with --path")
	err := withCategory(cause, protos.AitoolsErrorCategoryInvalidFlags)
	assert.Equal(t, cause.Error(), err.Error())
	assert.ErrorIs(t, err, cause)
	assert.NoError(t, withCategory(nil, protos.AitoolsErrorCategoryInvalidFlags))
	assert.NoError(t, promptError(nil))
}

// A corrupt install state must not be blamed on the remote manifest: the
// installer's typed StateError makes it STATE_INVALID.
func TestClassifyInstallErrorCorruptStateIsStateInvalid(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".state.json"), []byte("{not json"), 0o600))
	_, err := installer.LoadState(dir)
	require.Error(t, err)
	assert.Equal(t, protos.AitoolsErrorCategoryStateInvalid, classifyInstallError(fmt.Errorf("failed to load install state: %w", err)))
}

// readMissingFile returns the error the OS gives for a missing file, so the
// test covers the platform's own error value (syscall.ENOENT on Unix,
// ERROR_FILE_NOT_FOUND on Windows).
func readMissingFile(t *testing.T) error {
	_, err := os.ReadFile(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	return err
}

func TestClassifyCancelledManifestFetchIsCommandInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Cancel while the request is in flight, then wait for the client to give up.
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	t.Setenv("DATABRICKS_SKILLS_BASE_URL", server.URL)

	_, err := (&installer.GitHubManifestSource{}).FetchManifest(ctx, "v1")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, protos.AitoolsErrorCategoryCommandInterrupted, classifyInstallError(err))
}

func TestClassifyRealManifestFetchErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		// cutOff announces a longer body than is sent, so the connection
		// closes mid-body.
		cutOff bool
		want   protos.AitoolsErrorCategory
	}{
		// A captive portal or broken mirror answers 200 with something else.
		{name: "200 with HTML", status: http.StatusOK, body: "<html>login</html>", want: protos.AitoolsErrorCategoryManifestInvalid},
		{name: "200 with the wrong JSON shape", status: http.StatusOK, body: `{"skills": []}`, want: protos.AitoolsErrorCategoryManifestInvalid},
		{name: "200 with an empty body", status: http.StatusOK, body: "", want: protos.AitoolsErrorCategoryManifestInvalid},
		{name: "200 with incomplete JSON", status: http.StatusOK, body: `{"skills":`, want: protos.AitoolsErrorCategoryManifestInvalid},
		{name: "connection closed mid-body", status: http.StatusOK, body: `{"skills":`, cutOff: true, want: protos.AitoolsErrorCategorySkillsFetchFailed},
		{name: "404", status: http.StatusNotFound, want: protos.AitoolsErrorCategoryManifestNotFound},
		{name: "503", status: http.StatusServiceUnavailable, want: protos.AitoolsErrorCategorySkillsFetchFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.cutOff {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+100))
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			t.Setenv("DATABRICKS_SKILLS_BASE_URL", server.URL)

			_, err := (&installer.GitHubManifestSource{}).FetchManifest(t.Context(), "v1")
			require.Error(t, err)
			assert.Equal(t, tc.want, classifyInstallError(err))
		})
	}
}
