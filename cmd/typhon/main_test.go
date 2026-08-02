package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedPythonFlagOnlyAppliesToRun(t *testing.T) {
	err := run([]string{"--shared-python", "check", "unused.ty"})
	if err == nil || !strings.Contains(err.Error(), "only apply to run") {
		t.Fatalf("expected option scope error, got %v", err)
	}
}

func TestSharedPythonEnvironmentDoesNotBlockCheck(t *testing.T) {
	t.Setenv("TYPHON_SHARED_PYTHON", "1")
	path := filepath.Join("..", "..", "examples", "void_return.ty")
	if err := run([]string{"check", path}); err != nil {
		t.Fatal(err)
	}
}
