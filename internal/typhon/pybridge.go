package typhon

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed pybridge_worker.py
var embeddedPyBridgeWorker string

type PythonBridge struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	encoder *json.Encoder
	mu      sync.Mutex
}

type pyRequest struct {
	Op     string                 `json:"op"`
	Module string                 `json:"module,omitempty"`
	ID     int64                  `json:"id,omitempty"`
	Name   string                 `json:"name,omitempty"`
	Args   []pyWireValue          `json:"args,omitempty"`
	Kwargs map[string]pyWireValue `json:"kwargs,omitempty"`
}

type pyResponse struct {
	OK        bool        `json:"ok"`
	Value     pyWireValue `json:"value"`
	Error     string      `json:"error"`
	Traceback string      `json:"traceback"`
}

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

func newPythonBridge(root string) (*PythonBridge, error) {
	worker, err := findPythonWorker(root)
	if err != nil {
		return nil, err
	}
	python, err := findPythonExecutable(root)
	if err != nil {
		return nil, err
	}

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

	return &PythonBridge{
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReader(stdout),
		encoder: json.NewEncoder(stdin),
	}, nil
}

func (b *PythonBridge) Close() error {
	if b == nil || b.cmd == nil {
		return nil
	}
	_ = b.stdin.Close()
	return b.cmd.Wait()
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

	if err := b.encoder.Encode(request); err != nil {
		return NoneValue(), err
	}
	line, err := b.reader.ReadBytes('\n')
	if err != nil {
		return NoneValue(), err
	}
	var response pyResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return NoneValue(), err
	}
	if !response.OK {
		return NoneValue(), fmt.Errorf("python bridge error: %s", response.Error)
	}
	return pythonWireToValue(b, response.Value), nil
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
