package typhon

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestSharedPythonBridgeReusesDaemonAcrossConnections(t *testing.T) {
	root := repoRoot(t)
	python, err := findPythonExecutable(root)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := findPythonWorker(root)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("TYPHON_PYBRIDGE_STATE_DIR", t.TempDir())
	t.Setenv("TYPHON_PYBRIDGE_IDLE_SECONDS", "0.25")
	statePath := pythonDaemonStatePath(python, worker)

	first, err := newPythonBridge(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Ping(); err != nil {
		t.Fatal(err)
	}
	firstModule, err := first.ImportModule("math")
	if err != nil {
		t.Fatal(err)
	}
	firstState := readDaemonState(t, statePath)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := newPythonBridge(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Ping(); err != nil {
		t.Fatal(err)
	}
	secondModule, err := second.ImportModule("math")
	if err != nil {
		t.Fatal(err)
	}
	if firstModule.PyObject == nil || secondModule.PyObject == nil {
		t.Fatalf("expected Python module handles, got first=%v second=%v", firstModule, secondModule)
	}
	if firstModule.PyObject.ID != 1 || secondModule.PyObject.ID != 1 {
		t.Fatalf(
			"object handles leaked across client sessions: first=%d second=%d",
			firstModule.PyObject.ID,
			secondModule.PyObject.ID,
		)
	}
	secondState := readDaemonState(t, statePath)
	if firstState.PID != secondState.PID || firstState.Token != secondState.Token {
		t.Fatalf("shared bridge started a second daemon: first=%+v second=%+v", firstState, secondState)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("shared Python daemon did not expire after becoming idle: %s", statePath)
}

func TestSharedPythonBridgeRecoversFromStaleState(t *testing.T) {
	root := repoRoot(t)
	python, err := findPythonExecutable(root)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := findPythonWorker(root)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	t.Setenv("TYPHON_PYBRIDGE_STATE_DIR", stateDir)
	t.Setenv("TYPHON_PYBRIDGE_IDLE_SECONDS", "0.1")
	statePath := pythonDaemonStatePath(python, worker)
	stale := pyDaemonState{
		Address:  "127.0.0.1:1",
		PID:      999999,
		Protocol: pyBridgeProtocolVersion,
		Token:    "stale",
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	bridge, err := newPythonBridge(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Ping(); err != nil {
		t.Fatal(err)
	}
	recovered := readDaemonState(t, statePath)
	if recovered.Token == stale.Token || recovered.Address == stale.Address {
		t.Fatalf("stale daemon state was not replaced: %+v", recovered)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	waitForDaemonExit(t, statePath)
}

func readDaemonState(t *testing.T, path string) pyDaemonState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state pyDaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func waitForDaemonExit(t *testing.T, statePath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("shared Python daemon did not expire after becoming idle: %s", statePath)
}
