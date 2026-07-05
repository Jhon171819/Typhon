package main

import (
	"fmt"
	"os"

	"typhon/internal/typhon"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	warmPython := false
	filtered := args[:0]
	for _, arg := range args {
		if arg == "--warm-python" {
			warmPython = true
			continue
		}
		filtered = append(filtered, arg)
	}
	args = filtered

	if len(args) == 0 {
		return fmt.Errorf("usage: typhon [--warm-python] <run|check|bytecode> <file.ty>")
	}

	command := args[0]
	if command != "run" && command != "check" && command != "bytecode" {
		if len(args) == 1 {
			return typhon.RunFileWithOptions(args[0], typhon.RunOptions{WarmPython: warmPython})
		}
		return fmt.Errorf("usage: typhon [--warm-python] <run|check|bytecode> <file.ty>")
	}

	if warmPython && command != "run" {
		return fmt.Errorf("--warm-python only applies to run")
	}

	if len(args) != 2 {
		return fmt.Errorf("usage: typhon [--warm-python] %s <file.ty>", command)
	}

	switch command {
	case "run":
		return typhon.RunFileWithOptions(args[1], typhon.RunOptions{WarmPython: warmPython})
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
