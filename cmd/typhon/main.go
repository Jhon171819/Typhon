package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"typhon/internal/typhon"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := runWithContext(ctx, os.Args[1:]); err != nil && !(ctx.Err() != nil && errors.Is(err, context.Canceled)) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runWithContext(context.Background(), args)
}

func runWithContext(ctx context.Context, args []string) error {
	warmPython := cliEnvEnabled("TYPHON_WARM_PYTHON")
	sharedPython := cliEnvEnabled("TYPHON_SHARED_PYTHON")
	warmPythonFlag := false
	sharedPythonFlag := false
	filtered := args[:0]
	for _, arg := range args {
		if arg == "--warm-python" {
			warmPython = true
			warmPythonFlag = true
			continue
		}
		if arg == "--shared-python" {
			sharedPython = true
			sharedPythonFlag = true
			continue
		}
		filtered = append(filtered, arg)
	}
	args = filtered

	if len(args) == 0 {
		return fmt.Errorf("usage: typhon [--warm-python] [--shared-python] <run|check|bytecode> <file.ty>")
	}

	command := args[0]
	if (command == "version" || command == "--version") && len(args) == 1 {
		fmt.Printf("Typhon %s (Go VM, Python bridge protocol %d)\n", typhon.Version, typhon.PythonProtocolVersion)
		return nil
	}
	if command != "run" && command != "check" && command != "bytecode" {
		if len(args) == 1 {
			return typhon.RunFileWithOptions(args[0], typhon.RunOptions{Context: ctx, WarmPython: warmPython, SharedPython: sharedPython})
		}
		return fmt.Errorf("usage: typhon [--warm-python] [--shared-python] <run|check|bytecode> <file.ty>")
	}

	if (warmPythonFlag || sharedPythonFlag) && command != "run" {
		return fmt.Errorf("--warm-python and --shared-python only apply to run")
	}

	if len(args) != 2 {
		return fmt.Errorf("usage: typhon [--warm-python] [--shared-python] %s <file.ty>", command)
	}

	switch command {
	case "run":
		return typhon.RunFileWithOptions(args[1], typhon.RunOptions{Context: ctx, WarmPython: warmPython, SharedPython: sharedPython})
	case "check":
		return typhon.CheckFile(args[1])
	case "bytecode":
		text, err := typhon.BytecodeFile(args[1])
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func cliEnvEnabled(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}
