package root_test

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"

	"github.com/databricks/cli/cmd/root"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRoot(t *testing.T, built *[]string) *root.Tree {
	r := root.NewTree(t.Context())
	r.SetOut(io.Discard)
	r.SetErr(io.Discard)
	spec := func(name string) root.Spec {
		return root.Spec{
			Use:     name,
			Short:   name + " commands",
			Aliases: []string{name + "-alias"},
			Build: func(ctx context.Context, cmd *cobra.Command) {
				*built = append(*built, name)
				cmd.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
			},
		}
	}
	r.Add(spec("one"), spec("two"))
	return r
}

func TestExecuteBuildsOnlyWhatArgsResolveTo(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"no args", nil, nil},
		{"unknown command", []string{"unknown"}, nil},
		{"command", []string{"one"}, []string{"one"}},
		{"alias", []string{"one-alias", "child"}, []string{"one"}},
		{"subcommand", []string{"one", "child"}, []string{"one"}},
		{"flag with value before command", []string{"--profile", "two", "two", "child"}, []string{"two"}},
		{"help flag", []string{"two", "--help"}, []string{"two"}},
		{"help", []string{"help", "one"}, []string{"one", "two"}},
		{"help with flag value that looks like a terminator", []string{"help", "--profile", "--", "one"}, []string{"one", "two"}},
		{"completion", []string{cobra.ShellCompRequestCmd, "one", ""}, []string{"one", "two"}},
		{"completion after flag", []string{"--profile", "x", cobra.ShellCompNoDescRequestCmd, "o"}, []string{"one", "two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var built []string
			r := newTestRoot(t, &built)
			_ = r.Execute(t.Context(), tc.args)
			assert.ElementsMatch(t, tc.want, built)
		})
	}
}

// Cobra keeps the completion command it adds, so later completions must
// still build commands.
func TestRepeatedCompletion(t *testing.T) {
	var built []string
	r := newTestRoot(t, &built)
	_ = r.Execute(t.Context(), []string{cobra.ShellCompRequestCmd, "one", ""})
	built = nil
	r.Add(root.Spec{Use: "three", Build: func(context.Context, *cobra.Command) { built = append(built, "three") }})
	_ = r.Execute(t.Context(), []string{cobra.ShellCompRequestCmd, "three", ""})
	assert.Equal(t, []string{"three"}, built)
}

func TestPlaceholdersAreListed(t *testing.T) {
	var built []string
	r := newTestRoot(t, &built)
	cmd, _, err := root.BuildAll(t.Context(), r).Find([]string{"one", "child"})
	require.NoError(t, err)
	assert.Equal(t, "databricks one child", cmd.CommandPath())

	var unbuilt []string
	var out bytes.Buffer
	r = newTestRoot(t, &unbuilt)
	r.SetOut(&out)
	require.NoError(t, r.Execute(t.Context(), []string{"--help"}))
	assert.Contains(t, out.String(), "one commands")
	assert.Contains(t, out.String(), "two commands")
	assert.Empty(t, unbuilt)
}

// A command keeps the group the root assigned to its placeholder, for example
// when the root ungrouped it because its group has no visible commands.
func TestHiddenCommandInDroppedGroupRuns(t *testing.T) {
	r := root.NewTree(t.Context())
	r.SetOut(io.Discard)
	r.SetErr(io.Discard)
	r.Add(
		root.Spec{Use: "visible", GroupID: "a", Build: func(context.Context, *cobra.Command) {}},
		root.Spec{Use: "hidden", GroupID: "b", Hidden: true, Build: func(context.Context, *cobra.Command) {}},
	)
	r.AddGroups(cobra.Group{ID: "a", Title: "A"}, cobra.Group{ID: "b", Title: "B"})
	assert.NotPanics(t, func() { _ = r.Execute(t.Context(), []string{"hidden", "--help"}) })
}

func TestAddCopiesSpecFields(t *testing.T) {
	aliases := []string{"old"}
	r := root.NewTree(t.Context())
	r.SetOut(io.Discard)
	r.Add(root.Spec{Use: "cmd", Aliases: aliases, Build: func(context.Context, *cobra.Command) {}})
	aliases[0] = "new"
	require.NoError(t, r.Execute(t.Context(), []string{"old", "--help"}))
}

// Every spec field must reach the command, and changing it while building
// must panic. This fails if a field is added to Spec but not handled.
func TestSpecFields(t *testing.T) {
	specType := reflect.TypeFor[root.Spec]()
	for i := range specType.NumField() {
		field := specType.Field(i)
		if field.Name == "Build" {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			spec := root.Spec{Use: "cmd"}
			set := func(v reflect.Value) {
				switch v.Kind() {
				case reflect.String:
					v.SetString(v.String() + "x")
				case reflect.Bool:
					v.SetBool(!v.Bool())
				case reflect.Slice:
					v.Set(reflect.Append(v, reflect.ValueOf("x")))
				default:
					t.Fatalf("unhandled kind %s", v.Kind())
				}
			}
			set(reflect.ValueOf(&spec).Elem().Field(i))

			spec.Build = func(context.Context, *cobra.Command) {}
			cmd := root.NewCommand(t.Context(), spec)
			assert.Equal(t, reflect.ValueOf(spec).Field(i).Interface(), reflect.ValueOf(cmd).Elem().FieldByName(field.Name).Interface())

			spec.Build = func(_ context.Context, cmd *cobra.Command) { set(reflect.ValueOf(cmd).Elem().FieldByName(field.Name)) }
			assert.Panics(t, func() { root.NewCommand(t.Context(), spec) })
		})
	}
}
