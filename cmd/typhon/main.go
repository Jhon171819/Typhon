package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"typhon/internal/typhon"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	stopWatchdog := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(typhon.ShutdownExitTimeout())
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			fmt.Fprintln(os.Stderr, "Typhon shutdown deadline exceeded")
			os.Exit(1)
		}
	})
	defer stopWatchdog()
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
		return fmt.Errorf("usage: typhon <run|check|check-imports|bytecode|openapi|migrate> [arguments]")
	}

	command := args[0]
	if (command == "version" || command == "--version") && len(args) == 1 {
		fmt.Printf("Typhon %s (Go VM, Python bridge protocol %d)\n", typhon.Version, typhon.PythonProtocolVersion)
		return nil
	}
	if command != "run" && command != "check" && command != "check-imports" && command != "bytecode" && command != "openapi" && command != "migrate" {
		if len(args) == 1 {
			return typhon.RunFileWithOptions(args[0], typhon.RunOptions{Context: ctx, WarmPython: warmPython, SharedPython: sharedPython})
		}
		return fmt.Errorf("usage: typhon <run|check|check-imports|bytecode|openapi|migrate> [arguments]")
	}

	if (warmPythonFlag || sharedPythonFlag) && command != "run" {
		return fmt.Errorf("--warm-python and --shared-python only apply to run")
	}
	if command == "migrate" {
		if len(args) != 2 && len(args) != 3 {
			return fmt.Errorf("usage: typhon migrate <status|plan|up|down|seed> [directory]")
		}
		directory := "migrations"
		if args[1] == "seed" {
			directory = "seeds"
		}
		if len(args) == 3 {
			directory = args[2]
		}
		return typhon.RunMigrations(ctx, os.Getenv("DATABASE_URL"), directory, args[1], os.Stdout)
	}
	if command == "openapi" {
		if len(args) != 2 && len(args) != 3 {
			return fmt.Errorf("usage: typhon openapi <file.ty> [router]")
		}
		binding := "routes"
		if len(args) == 3 {
			binding = args[2]
		}
		document, err := typhon.OpenAPIFile(ctx, args[1], binding)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(document)
		return err
	}

	if len(args) != 2 {
		return fmt.Errorf("usage: typhon [--warm-python] [--shared-python] %s <file.ty>", command)
	}

	switch command {
	case "run":
		return typhon.RunFileWithOptions(args[1], typhon.RunOptions{Context: ctx, WarmPython: warmPython, SharedPython: sharedPython})
	case "check":
		return typhon.CheckFile(args[1])
	case "check-imports":
		return typhon.CheckImports(args[1])
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
