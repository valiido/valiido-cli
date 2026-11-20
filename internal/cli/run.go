package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		args = []string{"help"}
	}

	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintln(stdout, "valiido "+version)
		return 0
	}
	retries := 0
	ctx = context.WithValue(ctx, retryKey{}, &retries)
	out := &output{Writer: stdout, ctx: ctx, retries: &retries}
	prefix, path := leadingGlobalOptions(args)
	helpArgs, _ := helpAndOutputArgs(path)
	_, mode := helpAndOutputArgs(args)
	out.mode = mode
	var err error
	if helpArgs != nil {
		if helpArgs[0] == "--help" || helpArgs[0] == "help" {
			def := standaloneCommands["help"]
			var topics []string
			if helpArgs[0] == "help" {
				topics = helpArgs[1 : len(helpArgs)-1]
			}
			err = def.run(&invocation{ctx, out, stderr, def, topics, false})
			if err == nil {
				return 0
			}
		}
		if contextual, ok := contextualHelp(helpArgs); ok {
			fmt.Fprint(stdout, contextual)
			return 0
		}
		if err == nil {
			err = unknownHelpCommand(helpArgs)
		}
	} else if len(path) == 0 {
		err = usageError("Expected a command. Run valiido --help.")
	} else {
		if len(prefix) > 0 {
			i := 0
			for i < len(path) && (path[i] == "-" || !strings.HasPrefix(path[i], "-")) {
				i++
			}
			args = append(append(append([]string{}, path[:i]...), prefix...), path[i:]...)
		}
		err = run(ctx, args, out, stderr)
	}

	if err == nil {
		return 0
	}
	var commandError *apiError
	if !errors.As(err, &commandError) {
		commandError = failure("output", "Could not write the result.")
	}
	writeFailure(out, stderr, commandError, args, retries)
	return exitCode(commandError.Code)
}

func run(ctx context.Context, args []string, stdout *output, stderr io.Writer) error {
	command, remaining, resourceAlias, err := routeCommand(args)
	if err != nil {
		return err
	}
	if commandSupportsProfile(command.name) {
		stdout.profile = profileFromArgs(remaining)
	}
	call := &invocation{ctx, stdout, stderr, command, remaining, resourceAlias}
	err = command.run(call)
	if errors.Is(err, flag.ErrHelp) {
		writeMainHelp(stdout)
		return nil
	}
	return err
}

func commandSupportsProfile(name string) bool {
	switch name {
	case "help", "more", "setup", "version", "upgrade", "shell-completion", "_complete":
		return false
	default:
		return true
	}
}

func showVersion(call *invocation) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	finishOutput := outputFlags(flags, call.output)
	if rejectDuplicateFlags(call.args, flags) != nil || flags.Parse(call.args) != nil || flags.NArg() != 0 {
		return usageError("Use valiido version with an optional output format.")
	}
	if err := finishOutput(); err != nil {
		return err
	}
	return writeResponse(call.output, response{OK: true, Summary: "valiido " + version, Data: map[string]string{"version": version}})
}
