package commandflags

import (
	"flag"
	"strings"
)

const MetadataArgument = "--_irrigator_flag_metadata"

type CommandFlag struct {
	Name    string `json:"name"`
	Default string `json:"default"`
	Usage   string `json:"usage"`
	Boolean bool   `json:"boolean"`
}
type CommandDefinition struct {
	Name        string        `json:"name"`
	Flags       []CommandFlag `json:"flags"`
	Subcommands []string      `json:"subcommands,omitempty"`
	Hidden      bool          `json:"hidden,omitempty"`
}
type CommandMetadata struct{ Flags []CommandFlag }

func (e *CommandMetadata) Error() string { return "command flag metadata requested" }
func CommandFlagMetadata(fs *flag.FlagSet, args []string) error {
	if len(args) != 1 || args[0] != MetadataArgument {
		return nil
	}
	result := &CommandMetadata{Flags: []CommandFlag{}}
	fs.VisitAll(func(f *flag.Flag) {
		boolean := false
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
			boolean = b.IsBoolFlag()
		}
		result.Flags = append(result.Flags, CommandFlag{f.Name, f.DefValue, f.Usage, boolean})
	})
	return result
}
func ParseCommandFlags(fs *flag.FlagSet, args []string) error {
	if err := CommandFlagMetadata(fs, args); err != nil {
		return err
	}
	return fs.Parse(args)
}

// parseCommandArguments preserves the CLI's positional-before-flags convention
// while using the same FlagSet definitions exposed by machine-readable help.
func ParseCommandArguments(fs *flag.FlagSet, args []string) error {
	if err := CommandFlagMetadata(fs, args); err != nil {
		return err
	}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		f := fs.Lookup(name)
		if f == nil || strings.Contains(arg, "=") {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return fs.Parse(append(flags, positional...))
}
