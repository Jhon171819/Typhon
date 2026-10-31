package typhon

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed pybridge_worker.py
var embeddedPyBridgeWorker string

type PythonBridge struct {
	releaseMu      sync.Mutex
	pendingRelease []int64
	closeOnce      sync.Once
	closed         atomic.Bool
	closeErr       error
	timeout        time.Duration
	sequence       int64
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	reader         *bufio.Reader
	encoder        *json.Encoder
	close          func() error
	mu             sync.Mutex
}

type pyRequest struct {
	IDs      []int64                `json:"ids,omitempty"`
	Sequence int64                  `json:"sequence"`
	Op       string                 `json:"op"`
	Module   string                 `json:"module,omitempty"`
	ID       int64                  `json:"id,omitempty"`
	Name     string                 `json:"name,omitempty"`
	Args     []pyWireValue          `json:"args,omitempty"`
	Kwargs   map[string]pyWireValue `json:"kwargs,omitempty"`
	Token    string                 `json:"token,omitempty"`
}

type pyResponse struct {
	Sequence  int64       `json:"sequence"`
	ErrorType string      `json:"error_type"`
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

const pyBridgeProtocolVersion = 3
const maxBridgeMessageBytes = 16 * 1024 * 1024

type pyWireValue struct {
	Kind    string        `json:"kind"`
	Value   any           `json:"value,omitempty"`
	Items   []pyWireValue `json:"items,omitempty"`
	Entries []pyWireEntry `json:"entries,omitempty"`
	ID      int64         `json:"id,omitempty"`
	Repr    string        `json:"repr,omitempty"`
}

type pyWireEntry struct {
	Key   pyWireValue `json:"key"`
	Value pyWireValue `json:"value"`
}

type PyObject struct {
	released atomic.Bool
	Bridge   *PythonBridge
	ID       int64
	Repr     string
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
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			_ = stdout.Close()
			<-done
			return nil
		}
	}
	return bridge, nil
}

func (b *PythonBridge) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		if b.close != nil {
			b.closeErr = b.close()
		}
	})
	return b.closeErr
}

func (b *PythonBridge) ImportModule(name string) (Value, error) {
	return b.roundTrip(pyRequest{Op: "import", Module: trimPythonNamespace(name)})
}

func (b *PythonBridge) Ping() error {
	_, err := b.roundTrip(pyRequest{Op: "ping"})
	return err
}

func (b *PythonBridge) GetAttr(object *PyObject, name string) (Value, error) {
	if err := b.checkObject(object); err != nil {
		return NoneValue(), err
	}
	value, err := b.roundTrip(pyRequest{Op: "getattr", ID: object.ID, Name: name})
	runtime.KeepAlive(object)
	return value, err
}

func (b *PythonBridge) Call(object *PyObject, args []Value, kwargs map[string]Value) (Value, error) {
	if err := b.checkObject(object); err != nil {
		return NoneValue(), err
	}
	wireArgs := make([]pyWireValue, 0, len(args))
	for _, arg := range args {
		wire, err := b.valueToWire(arg, 0)
		if err != nil {
			return NoneValue(), err
		}
		wireArgs = append(wireArgs, wire)
	}
	wireKwargs := map[string]pyWireValue{}
	for name, value := range kwargs {
		wire, err := b.valueToWire(value, 0)
		if err != nil {
			return NoneValue(), err
		}
		wireKwargs[name] = wire
	}
	value, err := b.roundTrip(pyRequest{Op: "call", ID: object.ID, Args: wireArgs, Kwargs: wireKwargs})
	runtime.KeepAlive(object)
	runtime.KeepAlive(args)
	runtime.KeepAlive(kwargs)
	return value, err
}

func (b *PythonBridge) roundTrip(request pyRequest) (Value, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseMu.Lock()
	ids := b.pendingRelease
	b.pendingRelease = nil
	b.releaseMu.Unlock()
	if len(ids) > 0 {
		if _, err := b.exchangeLocked(pyRequest{Op: "release_many", IDs: ids}); err != nil {
			return NoneValue(), err
		}
	}
	return b.roundTripLocked(request)
}

func (b *PythonBridge) roundTripLocked(request pyRequest) (Value, error) {
	response, err := b.exchangeLocked(request)
	if err != nil {
		return NoneValue(), err
	}
	return pythonWireToValue(b, response.Value)
}

func (b *PythonBridge) exchangeLocked(request pyRequest) (pyResponse, error) {
	if b.closed.Load() {
		return pyResponse{}, fmt.Errorf("python bridge is closed")
	}
	b.sequence++
	request.Sequence = b.sequence
	type result struct {
		response pyResponse
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := b.exchangeIO(request); done <- result{response, err} }()
	timeout := b.timeout
	if timeout <= 0 {
		timeout = pythonCallTimeout()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		var pythonErr *PythonError
		if result.err != nil && !errors.As(result.err, &pythonErr) {
			_ = b.Close()
		}
		return result.response, result.err
	case <-timer.C:
		_ = b.Close()
		return pyResponse{}, fmt.Errorf("python bridge call %s timed out after %s; session closed", request.Op, timeout)
	}
}

func (b *PythonBridge) exchangeIO(request pyRequest) (pyResponse, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return pyResponse{}, err
	}
	if len(data) > maxBridgeMessageBytes {
		return pyResponse{}, fmt.Errorf("python bridge request size limit exceeded")
	}
	if err := b.encoder.Encode(request); err != nil {
		return pyResponse{}, err
	}
	line, err := readBridgeLine(b.reader)
	if err != nil {
		return pyResponse{}, err
	}
	var response pyResponse
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return pyResponse{}, err
	}
	if response.Sequence != request.Sequence {
		return pyResponse{}, fmt.Errorf("python bridge response sequence mismatch")
	}
	if !response.OK {
		return pyResponse{}, &PythonError{Type: response.ErrorType, Message: response.Error, Traceback: response.Traceback}
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

func (b *PythonBridge) valueToWire(value Value, depth int) (pyWireValue, error) {
	if depth > 64 {
		return pyWireValue{}, fmt.Errorf("python bridge value depth limit exceeded")
	}
	switch value.Kind {
	case ValueNone:
		return pyWireValue{Kind: "none"}, nil
	case ValueBool:
		return pyWireValue{Kind: "bool", Value: value.Bool}, nil
	case ValueInt:
		return pyWireValue{Kind: "int", Value: value.Int}, nil
	case ValueFloat:
		if math.IsInf(value.Float, 0) || math.IsNaN(value.Float) {
			return pyWireValue{}, fmt.Errorf("non-finite Python bridge float")
		}
		return pyWireValue{Kind: "float", Value: value.Float}, nil
	case ValueString:
		return pyWireValue{Kind: "str", Value: value.Str}, nil
	case ValueList:
		items := []pyWireValue{}
		if value.List != nil {
			for _, item := range *value.List {
				wire, err := b.valueToWire(item, depth+1)
				if err != nil {
					return pyWireValue{}, err
				}
				items = append(items, wire)
			}
		}
		return pyWireValue{Kind: "list", Items: items}, nil
	case ValueDict:
		entries := []pyWireEntry{}
		if value.Dict != nil {
			for _, entry := range value.Dict.Entries {
				key, err := b.valueToWire(entry.Key, depth+1)
				if err != nil {
					return pyWireValue{}, err
				}
				item, err := b.valueToWire(entry.Value, depth+1)
				if err != nil {
					return pyWireValue{}, err
				}
				entries = append(entries, pyWireEntry{Key: key, Value: item})
			}
		}
		return pyWireValue{Kind: "dict", Entries: entries}, nil
	case ValuePyObject:
		if err := b.checkObject(value.PyObject); err != nil {
			return pyWireValue{}, err
		}
		return pyWireValue{Kind: "object", ID: value.PyObject.ID}, nil
	default:
		return pyWireValue{}, fmt.Errorf("unsupported Python bridge value: %s", value.Kind)
	}
}

func pythonWireToValue(bridge *PythonBridge, value pyWireValue) (Value, error) {
	return pythonWireDepth(bridge, value, 0)
}

func pythonWireDepth(bridge *PythonBridge, value pyWireValue, depth int) (Value, error) {
	if depth > 64 {
		return NoneValue(), fmt.Errorf("Python bridge response depth limit exceeded")
	}
	switch value.Kind {
	case "none":
		return NoneValue(), nil
	case "bool":
		if typed, ok := value.Value.(bool); ok {
			return BoolValue(typed), nil
		}
	case "int":
		switch typed := value.Value.(type) {
		case json.Number:
			integer, err := typed.Int64()
			if err != nil {
				return NoneValue(), fmt.Errorf("Python integer outside int64 or invalid: %s", typed)
			}
			return IntValue(integer), nil
		case int64:
			return IntValue(typed), nil
		}
	case "float":
		if typed, ok := value.Value.(json.Number); ok {
			floating, err := typed.Float64()
			if err != nil || math.IsNaN(floating) || math.IsInf(floating, 0) {
				return NoneValue(), fmt.Errorf("invalid or non-finite Python float: %s", typed)
			}
			return FloatValue(floating), nil
		}
	case "str":
		if typed, ok := value.Value.(string); ok {
			return StringValue(typed), nil
		}
	case "list":
		items := make([]Value, 0, len(value.Items))
		for _, item := range value.Items {
			converted, err := pythonWireDepth(bridge, item, depth+1)
			if err != nil {
				return NoneValue(), err
			}
			items = append(items, converted)
		}
		return ListValue(items), nil
	case "dict":
		result := DictValue()
		for _, entry := range value.Entries {
			key, err := pythonWireDepth(bridge, entry.Key, depth+1)
			if err != nil {
				return NoneValue(), err
			}
			item, err := pythonWireDepth(bridge, entry.Value, depth+1)
			if err != nil {
				return NoneValue(), err
			}
			if err := result.Dict.put(key, item); err != nil {
				return NoneValue(), err
			}
		}
		return result, nil
	case "object":
		if value.ID <= 0 {
			return NoneValue(), fmt.Errorf("invalid Python handle")
		}
		object := &PyObject{Bridge: bridge, ID: value.ID, Repr: value.Repr}
		runtime.SetFinalizer(object, finalizePythonObject)
		return Value{Kind: ValuePyObject, PyObject: object}, nil
	}
	return NoneValue(), fmt.Errorf("invalid Python wire value of kind %q", value.Kind)
}

type PythonError struct{ Type, Message, Traceback string }

func (e *PythonError) Error() string {
	return fmt.Sprintf("python bridge error: %s: %s\n%s", e.Type, e.Message, e.Traceback)
}

func (b *PythonBridge) checkObject(object *PyObject) error {
	if object == nil || object.Bridge != b || object.released.Load() {
		return fmt.Errorf("invalid, released, or foreign Python handle")
	}
	return nil
}

func (b *PythonBridge) Release(object *PyObject) error {
	if err := b.checkObject(object); err != nil {
		return err
	}
	_, err := b.roundTrip(pyRequest{Op: "release", ID: object.ID})
	if err == nil {
		object.released.Store(true)
		runtime.SetFinalizer(object, nil)
	}
	return err
}

func finalizePythonObject(object *PyObject) {
	if object.Bridge == nil || object.released.Swap(true) || object.Bridge.closed.Load() {
		return
	}
	bridge := object.Bridge
	bridge.releaseMu.Lock()
	bridge.pendingRelease = append(bridge.pendingRelease, object.ID)
	bridge.releaseMu.Unlock()
}

func pythonCallTimeout() time.Duration {
	if value, err := time.ParseDuration(os.Getenv("TYPHON_PYTHON_TIMEOUT")); err == nil && value > 0 {
		return value
	}
	return 30 * time.Second
}

func readBridgeLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxBridgeMessageBytes {
			return nil, fmt.Errorf("python bridge response size limit exceeded")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func findPythonExecutable(root string) (string, error) {
	if configured := os.Getenv("TYPHON_PYTHON"); configured != "" {
		return configured, nil
	}

	var candidates []string
	for _, base := range candidateRoots(root) {
		candidates = append(candidates, filepath.Join(base, ".venv", "Scripts", "python.exe"), filepath.Join(base, ".venv", "bin", "python"))
	}
	candidates = append(candidates, "python", "python3")
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
