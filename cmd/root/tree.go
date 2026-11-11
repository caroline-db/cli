package root

import (
	"context"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"

	"github.com/databricks/cli/libs/cmdgroup"
	"github.com/spf13/cobra"
)

// Spec declares a command. Top-level commands are added to a [Tree] as specs,
// so that each is only built when an invocation needs it. Until then, the tree
// holds a placeholder with the spec's fields, which are what the root shows
// about the command in help, completion, and suggestions. Build completes a command
// that already has these fields, for example with its flags, run function,
// and subcommands, and must not change them.
type Spec struct {
	Use        string
	Short      string
	GroupID    string
	Aliases    []string
	SuggestFor []string
	Hidden     bool
	Deprecated string

	Build func(ctx context.Context, cmd *cobra.Command)
}

// command returns a command with the spec's fields.
func (s Spec) command() *cobra.Command {
	return &cobra.Command{
		Use:        s.Use,
		Short:      s.Short,
		GroupID:    s.GroupID,
		Aliases:    slices.Clone(s.Aliases),
		SuggestFor: slices.Clone(s.SuggestFor),
		Hidden:     s.Hidden,
		Deprecated: s.Deprecated,
	}
}

// fields returns the spec fields of cmd, without Build.
func fields(cmd *cobra.Command) Spec {
	return Spec{
		Use:        cmd.Use,
		Short:      cmd.Short,
		GroupID:    cmd.GroupID,
		Aliases:    cmd.Aliases,
		SuggestFor: cmd.SuggestFor,
		Hidden:     cmd.Hidden,
		Deprecated: cmd.Deprecated,
	}
}

// NewCommand builds the command that spec declares. Use it to add commands
// below the top level, which are built together with their parent.
func NewCommand(ctx context.Context, spec Spec) *cobra.Command {
	cmd := spec.command()
	spec.Build(ctx, cmd)
	// Placeholders show the spec's fields, so the command must show the same.
	if !reflect.DeepEqual(fields(cmd), fields(spec.command())) {
		panic(fmt.Sprintf("root: building %q changed fields that its spec declares; set them in the spec instead", spec.Use))
	}
	return cmd
}

// Tree is the CLI's command tree. It owns the root command, so commands can
// only be added as specs and the tree can only run through [Tree.Execute],
// which builds the commands that the args resolve to before running them.
type Tree struct {
	cmd     *cobra.Command
	pending map[*cobra.Command]Spec
}

// NewTree returns a tree with the CLI's root command and no subcommands.
func NewTree(ctx context.Context) *Tree {
	return &Tree{cmd: newCommand(ctx), pending: map[*cobra.Command]Spec{}}
}

// Add adds top-level commands. Each is built when an invocation needs it.
func (t *Tree) Add(specs ...Spec) {
	for _, spec := range specs {
		// Copy the fields so that the caller changing them later can't make
		// the placeholder and the built command differ.
		build := spec.Build
		spec = fields(spec.command())
		spec.Build = build
		placeholder := spec.command()
		// Cobra only lists a command in help and completion if it is
		// runnable or has subcommands; a placeholder has no subcommands.
		placeholder.RunE = func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("internal error: %s was not built before running", cmd.CommandPath())
		}
		t.cmd.AddCommand(placeholder)
		t.pending[placeholder] = spec
	}
}

// AddGroups adds the groups that have at least one available top-level
// command; see [cmdgroup.Configure].
func (t *Tree) AddGroups(groups ...cobra.Group) {
	cmdgroup.Configure(t.cmd, groups)
}

// SetOut sets the destination for the commands' output.
func (t *Tree) SetOut(w io.Writer) { t.cmd.SetOut(w) }

// SetErr sets the destination for the commands' error output.
func (t *Tree) SetErr(w io.Writer) { t.cmd.SetErr(w) }

// SetIn sets the source for the commands' input.
func (t *Tree) SetIn(in io.Reader) { t.cmd.SetIn(in) }

// Execute builds the commands that args resolve to and runs the command.
func (t *Tree) Execute(ctx context.Context, args []string) error {
	t.build(ctx, args)
	return execute(ctx, t.cmd, args)
}

// buildAll builds every command and returns the root command.
func (t *Tree) buildAll(ctx context.Context) *cobra.Command {
	for placeholder := range maps.Clone(t.pending) {
		t.replace(ctx, placeholder)
	}
	return t.cmd
}

// replace replaces placeholder with the command its spec builds.
func (t *Tree) replace(ctx context.Context, placeholder *cobra.Command) {
	spec := t.pending[placeholder]
	delete(t.pending, placeholder)
	cmd := NewCommand(ctx, spec)
	// The root may have ungrouped the placeholder, for example because its
	// group has no visible commands.
	cmd.GroupID = placeholder.GroupID
	t.cmd.RemoveCommand(placeholder)
	t.cmd.AddCommand(cmd)
}

// build builds the command that args resolve to. Cobra resolves the command
// from args before running any hook, so this must run before it executes.
func (t *Tree) build(ctx context.Context, args []string) {
	// Cobra adds its help and completion commands when executing. Add them now
	// so that args naming them resolve to them.
	t.cmd.InitDefaultHelpCmd()
	complete := &cobra.Command{
		Use:     cobra.ShellCompRequestCmd,
		Aliases: []string{cobra.ShellCompNoDescRequestCmd},
		Hidden:  true,
	}
	t.cmd.AddCommand(complete)
	defer t.cmd.RemoveCommand(complete)

	cmd, _, err := t.cmd.Find(args)
	if err != nil {
		return
	}
	switch {
	case cmd.Parent() == t.cmd && (cmd.Name() == "help" || isCompletion(cmd)):
		// Help and completion resolve commands from their own arguments, the
		// way cobra parses them. They are rare enough to build everything.
		t.buildAll(ctx)
	case t.isPending(cmd):
		t.replace(ctx, cmd)
	}
}

func (t *Tree) isPending(cmd *cobra.Command) bool {
	_, ok := t.pending[cmd]
	return ok
}

func isCompletion(cmd *cobra.Command) bool {
	return cmd.Name() == cobra.ShellCompRequestCmd
}
