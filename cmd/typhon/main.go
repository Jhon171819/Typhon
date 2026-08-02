package main

import (
	"fmt"
	"os"
	"strings"

	"typhon/internal/typhon"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
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
	if command != "run" && command != "check" && command != "bytecode" {
		if len(args) == 1 {
			return typhon.RunFileWithOptions(args[0], typhon.RunOptions{WarmPython: warmPython, SharedPython: sharedPython})
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
		return typhon.RunFileWithOptions(args[1], typhon.RunOptions{WarmPython: warmPython, SharedPython: sharedPython})
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
