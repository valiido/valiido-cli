package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var resourceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var commandNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}\.[a-z][a-z0-9_]{0,63}$`)
var positiveIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
var optionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func usageError(message string) error { return failure("usage", message) }

// Split off leading global options so command routing still sees the noun
// and positional operands first. Leaf parsers validate both sets together.
func leadingGlobalOptions(args []string) ([]string, []string) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		name, _, inline := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch name {
		case "profile", "jq":
			if !inline && i+1 < len(args) {
				i++
			}
		case "json", "quiet", "ids-only", "count", "markdown", "styled", "dry-run":
		default:
			return args[:i], args[i:]
		}
		i++
	}
	return args[:i], args[i:]
}

func visitArguments(args []string, visit func(int, string, string)) {
	for i := 0; i < len(args); i++ {
		raw := args[i]
		if raw == "--" {
			return
		}
		if !strings.HasPrefix(raw, "-") || raw == "-" {
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(raw, "-"), "=")
		switch name {
		case "profile", "query", "jq", "limit", "cursor", "page-size", "input", "input-file", "output", "file", "agent", "server":
			if !inline && i+1 < len(args) {
				value = args[i+1]
				i++
			}
		}
		visit(i, name, value)
	}
}

func helpAndOutputArgs(args []string) ([]string, string) {
	var helpArgs []string
	var mode string
	visitArguments(args, func(index int, name, value string) {
		switch name {
		case "h", "help":
			if value == "" || value == "true" {
				helpArgs = append(append([]string(nil), args[:index]...), "--help")
			}
		case "json", "styled", "markdown", "quiet", "ids-only", "count", "jq":
			if (name == "jq" || value == "" || value == "true") && (mode == "" || name == "json") {
				mode = name
			}
		}
	})
	return helpArgs, mode
}

func profileFromArgs(args []string) string {
	var profile string
	visitArguments(args, func(_ int, name, value string) {
		if name == "profile" && profilePattern.MatchString(value) {
			profile = value
		}
	})
	return profile
}

func (call *invocation) parse(bind func(*flag.FlagSet), beforeOutput func() error) (*flag.FlagSet, error) {
	flags := flag.NewFlagSet(call.command.name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "default", "Credential profile")
	finishOutput := outputFlags(flags, call.output)
	if bind != nil {
		bind(flags)
	}
	if err := rejectDuplicateFlags(call.args, flags); err != nil {
		return nil, err
	}
	if err := flags.Parse(call.args); errors.Is(err, flag.ErrHelp) {
		return nil, err
	} else if err != nil {
		return nil, optionParseError(err, flags)
	}
	if flags.NArg() != 0 {
		return nil, usageError("Unexpected positional arguments. Usage: " + call.command.usage)
	}
	if !profilePattern.MatchString(*profile) {
		return nil, usageError("--profile must contain 1 to 64 letters, numbers, hyphens or underscores.")
	}
	call.output.profile = *profile
	if err := validateOptions(beforeOutput); err != nil {
		return nil, err
	}
	if err := finishOutput(); err != nil {
		return nil, err
	}
	return flags, nil
}

func optionParseError(err error, flags *flag.FlagSet) error {
	message := err.Error()
	for _, issue := range []struct{ prefix, message string }{
		{"flag needs an argument: -", "Expected a value for --"},
		{"flag provided but not defined: -", "Unknown option --"},
	} {
		if name, ok := strings.CutPrefix(message, issue.prefix); ok && optionNamePattern.MatchString(name) {
			return usageError(issue.message + name + ".")
		}
	}
	if _, tail, ok := strings.Cut(message, " for flag -"); ok {
		name, _, _ := strings.Cut(tail, ":")
		if flags.Lookup(name) != nil {
			return usageError("Invalid value for --" + name + ".")
		}
	}
	return usageError("Invalid options.")
}

func validateOptions(validate func() error) error {
	if validate == nil {
		return nil
	}
	return validate()
}

func (call *invocation) takeArgument(message string) (string, error) {
	if len(call.args) == 0 || strings.TrimSpace(call.args[0]) == "" {
		return "", usageError(message)
	}
	value := call.args[0]
	call.args = call.args[1:]
	return value, nil
}

func (call *invocation) takeResourceID() (int64, error) {
	if len(call.args) == 0 {
		return 0, usageError("Expected a positive resource ID before options.")
	}
	value := call.args[0]
	call.args = call.args[1:]
	return parseResourceID(value)
}

func optionProvided(flags *flag.FlagSet, name string) bool {
	provided := false
	flags.Visit(func(option *flag.Flag) { provided = provided || option.Name == name })
	return provided
}

func (call *invocation) parseListOptions(maximum int, paginated bool) (listOptions, error) {
	options := listOptions{pageSize: pageSize}
	flags, err := call.parse(func(flags *flag.FlagSet) {
		flags.StringVar(&options.query, "query", "", "Filter results")
		flags.IntVar(&options.limit, "limit", 0, "Maximum number of results")
		if paginated {
			flags.StringVar(&options.cursor, "cursor", "", "Resume from a previous list response")
			flags.IntVar(&options.pageSize, "page-size", pageSize, "Maximum number of results per request")
		}
	}, nil)
	if err != nil {
		return options, err
	}
	switch {
	case optionProvided(flags, "limit") && (options.limit < 1 || options.limit > maximum):
		return options, usageError(fmt.Sprintf("--limit must be between 1 and %d.", maximum))
	case options.pageSize < 1 || options.pageSize > pageSize:
		return options, usageError(fmt.Sprintf("--page-size must be between 1 and %d.", pageSize))
	case len(options.cursor) > maxCursorBytes:
		return options, usageError("--cursor exceeds the maximum length.")
	case len(options.query) > 1024:
		return options, usageError("--query cannot exceed 1024 bytes.")
	}
	return options, nil
}

func rejectDuplicateFlags(args []string, flags *flag.FlagSet) error {
	seen := map[string]bool{}
	for index := 0; index < len(args); index++ {
		raw := args[index]
		if raw == "--" {
			break
		}
		if !strings.HasPrefix(raw, "-") || raw == "-" {
			continue
		}
		nameAndValue := strings.TrimLeft(raw, "-")
		name, _, hasValue := strings.Cut(nameAndValue, "=")
		option := flags.Lookup(name)
		if option == nil {
			continue
		}
		if seen[name] {
			return failure("usage", fmt.Sprintf("Option --%s may only be specified once.", name))
		}
		seen[name] = true
		boolean, isBoolean := option.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && (!isBoolean || !boolean.IsBoolFlag()) {
			index++
		}
	}
	return nil
}

func parseResourceID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, failure("usage", "Resource ID must be an integer from 1 to 9223372036854775807.")
	}
	return id, nil
}
