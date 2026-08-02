package typhon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Engine struct {
	modules      map[string]*LoadedModule
	infos        map[string]*ModuleInfo
	root         string
	bridge       *PythonBridge
	bridgeErr    error
	bridgeDone   chan struct{}
	bridgeMu     sync.Mutex
	warmPython   bool
	sharedPython bool
}

type LoadedModule struct {
	Path     string
	Name     string
	Program  *Program
	Info     *ModuleInfo
	Globals  map[string]Value
	Executed bool
	Engine   *Engine
}

func NewEngine() *Engine {
	return &Engine{
		modules: map[string]*LoadedModule{},
		infos:   map[string]*ModuleInfo{},
	}
}

func RunFile(path string) error {
	return RunFileWithOptions(path, RunOptions{
		WarmPython:   envEnabled("TYPHON_WARM_PYTHON"),
		SharedPython: envEnabled("TYPHON_SHARED_PYTHON"),
	})
}

func RunFileWithOptions(path string, options RunOptions) error {
	engine := NewEngine()
	engine.warmPython = options.WarmPython
	engine.sharedPython = options.SharedPython
	defer engine.Close()
	module, err := engine.LoadModule(path)
	if err != nil {
		return err
	}
	return engine.ExecuteModule(module, true)
}

func CheckFile(path string) error {
	engine := NewEngine()
	defer engine.Close()
	_, err := engine.LoadModule(path)
	return err
}

func BytecodeFile(path string) (string, error) {
	engine := NewEngine()
	defer engine.Close()
	module, err := engine.LoadModule(path)
	if err != nil {
		return "", err
	}
	return Disassemble(module.Program), nil
}

func (e *Engine) LoadModule(path string) (*LoadedModule, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if module, ok := e.modules[abs]; ok {
		return module, nil
	}
	if e.root == "" {
		e.root = projectRoot(filepath.Dir(abs))
	}
	if e.warmPython {
		e.StartPythonBridgeWarmup()
	}
	source, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	ast, err := ParseFile(abs, string(source))
	if err != nil {
		return nil, err
	}
	info, err := CheckModule(ast, e)
	if err != nil {
		return nil, err
	}
	program, err := CompileModule(ast)
	if err != nil {
		return nil, err
	}
	module := &LoadedModule{
		Path:    abs,
		Name:    moduleNameFromPath(abs),
		Program: program,
		Info:    info,
		Globals: runtimeGlobals(),
		Engine:  e,
	}
	e.modules[abs] = module
	e.infos[abs] = info
	return module, nil
}

type RunOptions struct {
	WarmPython   bool
	SharedPython bool
}

func (e *Engine) ResolveImport(module string, fromDir string) (*ModuleInfo, error) {
	if module == "typhon.parallel" {
		return parallelModuleInfo(), nil
	}
	if isPythonImport(module) {
		return pythonModuleInfo(module), nil
	}
	path, err := resolveModulePath(module, fromDir)
	if err != nil {
		return nil, err
	}
	loaded, err := e.LoadModule(path)
	if err != nil {
		return nil, err
	}
	return loaded.Info, nil
}

func (e *Engine) LoadModuleByName(module string, fromDir string) (*LoadedModule, error) {
	if module == "typhon.parallel" {
		return &LoadedModule{
			Path:     "<typhon.parallel>",
			Name:     "typhon.parallel",
			Info:     parallelModuleInfo(),
			Globals:  runtimeGlobals(),
			Executed: true,
			Engine:   e,
		}, nil
	}
	if isPythonImport(module) {
		return nil, fmt.Errorf("python module %q is handled by the Python bridge", module)
	}
	path, err := resolveModulePath(module, fromDir)
	if err != nil {
		return nil, err
	}
	loaded, err := e.LoadModule(path)
	if err != nil {
		return nil, err
	}
	if !loaded.Executed {
		if err := e.ExecuteModule(loaded, false); err != nil {
			return nil, err
		}
	}
	return loaded, nil
}

func (e *Engine) PythonBridge() (*PythonBridge, error) {
	e.bridgeMu.Lock()
	if e.bridge != nil {
		bridge := e.bridge
		e.bridgeMu.Unlock()
		return bridge, nil
	}
	if e.bridgeDone != nil {
		done := e.bridgeDone
		e.bridgeMu.Unlock()
		<-done
		e.bridgeMu.Lock()
		defer e.bridgeMu.Unlock()
		if e.bridge != nil {
			return e.bridge, nil
		}
		if e.bridgeErr != nil {
			return nil, e.bridgeErr
		}
	}
	e.bridgeMu.Unlock()

	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()
	if e.bridge != nil {
		return e.bridge, nil
	}
	root := e.root
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = projectRoot(cwd)
		}
	}
	bridge, err := newPythonBridge(root, e.sharedPython)
	if err != nil {
		return nil, err
	}
	e.bridge = bridge
	return bridge, nil
}

func (e *Engine) StartPythonBridgeWarmup() {
	e.bridgeMu.Lock()
	if e.bridge != nil || e.bridgeDone != nil {
		e.bridgeMu.Unlock()
		return
	}
	done := make(chan struct{})
	e.bridgeDone = done
	root := e.root
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = projectRoot(cwd)
		}
	}
	e.bridgeMu.Unlock()

	go func() {
		bridge, err := newPythonBridge(root, e.sharedPython)
		if err == nil {
			err = bridge.Ping()
			if err != nil {
				_ = bridge.Close()
				bridge = nil
			}
		}
		e.bridgeMu.Lock()
		if err != nil {
			e.bridgeErr = err
		} else {
			e.bridge = bridge
		}
		close(done)
		e.bridgeMu.Unlock()
	}()
}

func (e *Engine) WarmPythonBridge() error {
	bridge, err := e.PythonBridge()
	if err != nil {
		return err
	}
	return bridge.Ping()
}

func (e *Engine) Close() error {
	e.bridgeMu.Lock()
	done := e.bridgeDone
	e.bridgeMu.Unlock()
	if done != nil {
		<-done
	}
	e.bridgeMu.Lock()
	bridge := e.bridge
	e.bridgeMu.Unlock()
	if bridge == nil {
		return nil
	}
	return bridge.Close()
}

func (e *Engine) ExecuteModule(module *LoadedModule, main bool) error {
	if module.Executed {
		return nil
	}
	module.Globals["__file__"] = StringValue(module.Path)
	if main {
		module.Globals["__name__"] = StringValue("__main__")
	} else {
		module.Globals["__name__"] = StringValue(module.Name)
	}
	vm := &VM{engine: e, module: module}
	_, err := vm.runFunction(module.Program.Module, nil)
	if err != nil {
		return err
	}
	module.Executed = true
	return nil
}

func resolveModulePath(module string, fromDir string) (string, error) {
	parts := strings.Split(module, ".")
	base := filepath.Join(append([]string{fromDir}, parts...)...)
	filePath := base + ".ty"
	if _, err := os.Stat(filePath); err == nil {
		return filePath, nil
	}
	initPath := filepath.Join(base, "__init__.ty")
	if _, err := os.Stat(initPath); err == nil {
		return initPath, nil
	}
	return "", fmt.Errorf("cannot import module %q from %s", module, fromDir)
}

func moduleNameFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func projectRoot(start string) string {
	abs, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return start
		}
		abs = parent
	}
}

func importBindingName(name string) string {
	name = trimPythonNamespace(name)
	parts := strings.Split(name, ".")
	if len(parts) == 0 {
		return name
	}
	return parts[0]
}

func parallelModuleInfo() *ModuleInfo {
	return &ModuleInfo{
		Path: "<typhon.parallel>",
		Exports: map[string]Type{
			"TyphonTask": TaskType(),
			"spawn":      {Kind: TypeFunc, Name: "spawn", Params: []Type{AnyType()}, Return: typePtr(TaskType())},
			"join":       {Kind: TypeFunc, Name: "join", Params: []Type{TaskType()}, Return: typePtr(AnyType())},
		},
		Classes: map[string]*ClassInfo{},
	}
}

func pythonModuleInfo(module string) *ModuleInfo {
	return &ModuleInfo{
		Path:    "<" + module + ">",
		Exports: map[string]Type{},
		Classes: map[string]*ClassInfo{},
	}
}

func envEnabled(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}
