package typhon

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed pybridge_worker.py
var embeddedPyBridgeWorker string

type PythonBridge struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	encoder *json.Encoder
	close   func() error
	mu      sync.Mutex
}

type pyRequest struct {
	Op     string                 `json:"op"`
	Module string                 `json:"module,omitempty"`
	ID     int64                  `json:"id,omitempty"`
	Name   string                 `json:"name,omitempty"`
	Args   []pyWireValue          `json:"args,omitempty"`
	Kwargs map[string]pyWireValue `json:"kwargs,omitempty"`
	Token  string                 `json:"token,omitempty"`
}

type pyResponse struct {
	OK        bool        `json:"ok"`
	Value     pyWireValue `json:"value"`
	Error     string      `json:"error"`
	Traceback string      `json:"traceback"`
	Protocol  int         `json:"protocol,omitempty"`
}

type pyDaemonState struct {
	Address  string `json:"address"`
	PID      int    `json:"pid"`
	Protocol int    `json:"protocol"`
	Token    string `json:"token"`
}

const pyBridgeProtocolVersion = 1

type pyWireValue struct {
	Kind  string        `json:"kind"`
	Value any           `json:"value,omitempty"`
	Items []pyWireValue `json:"items,omitempty"`
	ID    int64         `json:"id,omitempty"`
	Repr  string        `json:"repr,omitempty"`
}

type PyObject struct {
	Bridge *PythonBridge
	ID     int64
	Repr   string
}

func newPythonBridge(root string, shared bool) (*PythonBridge, error) {
	worker, err := findPythonWorker(root)
	if err != nil {
		return nil, err
	}
	python, err := findPythonExecutable(root)
	if err != nil {
		return nil, err
	}
	if shared {
		return newSharedPythonBridge(python, worker)
	}
	return newPrivatePythonBridge(python, worker)
}

func newPrivatePythonBridge(python string, worker string) (*PythonBridge, error) {
	cmd := exec.Command(python, worker)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	bridge := &PythonBridge{
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReader(stdout),
		encoder: json.NewEncoder(stdin),
	}
	bridge.close = func() error {
		_ = stdin.Close()
		return cmd.Wait()
	}
	return bridge, nil
}

func (b *PythonBridge) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.close == nil {
		return nil
	}
	closeBridge := b.close
	b.close = nil
	return closeBridge()
}

func (b *PythonBridge) ImportModule(name string) (Value, error) {
	return b.roundTrip(pyRequest{Op: "import", Module: trimPythonNamespace(name)})
}

func (b *PythonBridge) Ping() error {
	_, err := b.roundTrip(pyRequest{Op: "ping"})
	return err
}

func (b *PythonBridge) GetAttr(object *PyObject, name string) (Value, error) {
	return b.roundTrip(pyRequest{Op: "getattr", ID: object.ID, Name: name})
}

func (b *PythonBridge) Call(object *PyObject, args []Value, kwargs map[string]Value) (Value, error) {
	wireArgs := make([]pyWireValue, 0, len(args))
	for _, arg := range args {
		wireArgs = append(wireArgs, valueToPythonWire(arg))
	}
	wireKwargs := map[string]pyWireValue{}
	for name, value := range kwargs {
		wireKwargs[name] = valueToPythonWire(value)
	}
	return b.roundTrip(pyRequest{Op: "call", ID: object.ID, Args: wireArgs, Kwargs: wireKwargs})
}

func (b *PythonBridge) roundTrip(request pyRequest) (Value, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.roundTripLocked(request)
}

func (b *PythonBridge) roundTripLocked(request pyRequest) (Value, error) {
	response, err := b.exchangeLocked(request)
	if err != nil {
		return NoneValue(), err
	}
	return pythonWireToValue(b, response.Value), nil
}

func (b *PythonBridge) exchangeLocked(request pyRequest) (pyResponse, error) {
	if err := b.encoder.Encode(request); err != nil {
		return pyResponse{}, err
	}
	line, err := b.reader.ReadBytes('\n')
	if err != nil {
		return pyResponse{}, err
	}
	var response pyResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return pyResponse{}, err
	}
	if !response.OK {
		return pyResponse{}, fmt.Errorf("python bridge error: %s", response.Error)
	}
	return response, nil
}

func newSharedPythonBridge(python string, worker string) (*PythonBridge, error) {
	statePath := pythonDaemonStatePath(python, worker)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, fmt.Errorf("create python bridge state directory: %w", err)
	}
	if bridge, err := connectPythonDaemon(statePath); err == nil {
		return bridge, nil
	}

	lockPath := statePath + ".lock"
	release, err := acquireDaemonLock(lockPath, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("python bridge daemon startup lock: %w", err)
	}
	defer release()

	if bridge, err := connectPythonDaemon(statePath); err == nil {
		return bridge, nil
	}
	_ = os.Remove(statePath)

	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("python bridge daemon token: %w", err)
	}
	idleSeconds := pythonDaemonIdleSeconds()
	cmd := exec.Command(
		python,
		worker,
		"--daemon",
		"--state-file", statePath,
		"--token", token,
		"--idle-seconds", strconv.FormatFloat(idleSeconds.Seconds(), 'f', -1, 64),
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start python bridge daemon: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		bridge, connectErr := connectPythonDaemon(statePath)
		if connectErr == nil {
			return bridge, nil
		}
		lastErr = connectErr
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("python bridge daemon did not become ready: %w", lastErr)
}

func connectPythonDaemon(statePath string) (*PythonBridge, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var state pyDaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Protocol != pyBridgeProtocolVersion || state.Address == "" || state.Token == "" {
		return nil, fmt.Errorf("incompatible python bridge daemon state")
	}
	connection, err := net.DialTimeout("tcp", state.Address, time.Second)
	if err != nil {
		return nil, err
	}
	bridge := &PythonBridge{
		stdin:   connection,
		reader:  bufio.NewReader(connection),
		encoder: json.NewEncoder(connection),
		close:   connection.Close,
	}
	bridge.mu.Lock()
	response, err := bridge.exchangeLocked(pyRequest{Op: "hello", Token: state.Token})
	bridge.mu.Unlock()
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if response.Protocol != pyBridgeProtocolVersion {
		_ = connection.Close()
		return nil, fmt.Errorf("python bridge daemon protocol %d, expected %d", response.Protocol, pyBridgeProtocolVersion)
	}
	return bridge, nil
}

func pythonDaemonStatePath(python string, worker string) string {
	stateDir := os.Getenv("TYPHON_PYBRIDGE_STATE_DIR")
	if stateDir == "" {
		stateDir = os.TempDir()
	}
	workerIdentity := worker
	if contents, err := os.ReadFile(worker); err == nil {
		workerDigest := sha256.Sum256(contents)
		workerIdentity += "\x00" + hex.EncodeToString(workerDigest[:])
	}
	identity := python + "\x00" + workerIdentity + "\x00" + strconv.Itoa(pyBridgeProtocolVersion)
	digest := sha256.Sum256([]byte(identity))
	name := "typhon-pybridge-" + hex.EncodeToString(digest[:8]) + ".json"
	return filepath.Join(stateDir, name)
}

func pythonDaemonIdleSeconds() time.Duration {
	value := strings.TrimSpace(os.Getenv("TYPHON_PYBRIDGE_IDLE_SECONDS"))
	if value == "" {
		return 5 * time.Minute
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(seconds * float64(time.Second))
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func acquireDaemonLock(path string, staleAfter time.Duration) (func(), error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > staleAfter {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func valueToPythonWire(value Value) pyWireValue {
	switch value.Kind {
	case ValueNone:
		return pyWireValue{Kind: "none"}
	case ValueBool:
		return pyWireValue{Kind: "bool", Value: value.Bool}
	case ValueInt:
		return pyWireValue{Kind: "int", Value: value.Int}
	case ValueFloat:
		return pyWireValue{Kind: "float", Value: value.Float}
	case ValueString:
		return pyWireValue{Kind: "str", Value: value.Str}
	case ValueList:
		items := []pyWireValue{}
		if value.List != nil {
			for _, item := range *value.List {
				items = append(items, valueToPythonWire(item))
			}
		}
		return pyWireValue{Kind: "list", Items: items}
	case ValuePyObject:
		return pyWireValue{Kind: "object", ID: value.PyObject.ID}
	default:
		return pyWireValue{Kind: "str", Value: value.String()}
	}
}

func pythonWireToValue(bridge *PythonBridge, value pyWireValue) Value {
	switch value.Kind {
	case "none":
		return NoneValue()
	case "bool":
		if typed, ok := value.Value.(bool); ok {
			return BoolValue(typed)
		}
	case "int":
		switch typed := value.Value.(type) {
		case float64:
			return IntValue(int64(typed))
		case int64:
			return IntValue(typed)
		}
	case "float":
		if typed, ok := value.Value.(float64); ok {
			return FloatValue(typed)
		}
	case "str":
		if typed, ok := value.Value.(string); ok {
			return StringValue(typed)
		}
	case "list":
		items := make([]Value, 0, len(value.Items))
		for _, item := range value.Items {
			items = append(items, pythonWireToValue(bridge, item))
		}
		return ListValue(items)
	case "object":
		return Value{Kind: ValuePyObject, PyObject: &PyObject{Bridge: bridge, ID: value.ID, Repr: value.Repr}}
	}
	return NoneValue()
}

func findPythonExecutable(root string) (string, error) {
	if configured := os.Getenv("TYPHON_PYTHON"); configured != "" {
		return configured, nil
	}

	candidates := []string{
		filepath.Join(root, ".venv", "Scripts", "python.exe"),
		filepath.Join(root, ".venv", "bin", "python"),
		"python",
		"python3",
	}
	for _, candidate := range candidates {
		if strings.Contains(candidate, string(filepath.Separator)) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("python bridge could not find Python; set TYPHON_PYTHON")
}

func findPythonWorker(root string) (string, error) {
	if configured := os.Getenv("TYPHON_PYBRIDGE_WORKER"); configured != "" {
		return configured, nil
	}
	for _, base := range candidateRoots(root) {
		path := filepath.Join(base, "internal", "typhon", "pybridge_worker.py")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	path := filepath.Join(os.TempDir(), "typhon_pybridge_worker.py")
	if err := os.WriteFile(path, []byte(embeddedPyBridgeWorker), 0o600); err != nil {
		return "", fmt.Errorf("python bridge worker not found and embedded worker could not be written: %w", err)
	}
	return path, nil
}

func candidateRoots(root string) []string {
	seen := map[string]bool{}
	var roots []string
	add := func(path string) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return
		}
		for {
			if !seen[abs] {
				seen[abs] = true
				roots = append(roots, abs)
			}
			parent := filepath.Dir(abs)
			if parent == abs {
				break
			}
			abs = parent
		}
	}
	add(root)
	if cwd, err := os.Getwd(); err == nil {
		add(cwd)
	}
	return roots
}

func trimPythonNamespace(name string) string {
	return strings.TrimPrefix(name, "py.")
}

func isPythonImport(name string) bool {
	return strings.HasPrefix(name, "py.")
}
