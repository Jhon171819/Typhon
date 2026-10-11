package typhon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Engine struct {
	resources resourceScope
	// VM execution is serialized. join yields this lock while its child runs.
	execMu       sync.Mutex
	moduleMu     sync.Mutex
	loading      map[string]bool
	tasks        []*Task
	ctx          context.Context
	cancel       context.CancelFunc
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
	Executing  bool
	ExecuteErr error
	Path       string
	Name       string
	Program    *Program
	Info       *ModuleInfo
	Globals    map[string]Value
	Executed   bool
	Engine     *Engine
}

func NewEngine() *Engine {
	return NewEngineWithContext(context.Background())
}

func NewEngineWithContext(parent context.Context) *Engine {
	ctx, cancel := context.WithCancel(parent)
	return &Engine{
		ctx: ctx, cancel: cancel, loading: map[string]bool{},
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
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	engine := NewEngineWithContext(ctx)
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
	e.moduleMu.Lock()
	defer e.moduleMu.Unlock()
	return e.loadModule(path)
}

func (e *Engine) loadModule(path string) (*LoadedModule, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if module, ok := e.modules[abs]; ok {
		return module, nil
	}
	if e.loading[abs] {
		return nil, fmt.Errorf("cyclic Typhon import: %s", abs)
	}
	e.loading[abs] = true
	defer delete(e.loading, abs)
	if e.root == "" {
		e.bridgeMu.Lock()
		e.root = projectRoot(filepath.Dir(abs))
		e.bridgeMu.Unlock()
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
	info, err := CheckModule(ast, lockedImportResolver{e})
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

type lockedImportResolver struct{ engine *Engine }

func (r lockedImportResolver) ResolveImport(module, fromDir string) (*ModuleInfo, error) {
	path, err := resolveModulePath(module, fromDir)
	if err != nil {
		return nil, err
	}
	loaded, err := r.engine.loadModule(path)
	if err != nil {
		return nil, err
	}
	return loaded.Info, nil
}

type RunOptions struct {
	Context      context.Context
	WarmPython   bool
	SharedPython bool
}

func (e *Engine) ResolveImport(module string, fromDir string) (*ModuleInfo, error) {
	if info, ok := nativeDriverInfo(module); ok {
		return info, nil
	}
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
	e.execMu.Lock()
	defer e.execMu.Unlock()
	return e.loadModuleByNameLocked(module, fromDir)
}

func (e *Engine) loadModuleByNameLocked(module string, fromDir string) (*LoadedModule, error) {
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
		if err := e.executeModuleLocked(loaded, false); err != nil {
			return nil, err
		}
	}
	return loaded, nil
}

func (e *Engine) PythonBridge() (*PythonBridge, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
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
		if e.bridge != nil {
			bridge := e.bridge
			e.bridgeMu.Unlock()
			return bridge, nil
		}
		if e.bridgeErr != nil {
			err := e.bridgeErr
			e.bridgeMu.Unlock()
			return nil, err
		}
	}
	e.bridgeMu.Unlock()

	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
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
	if e.ctx.Err() != nil {
		e.bridgeMu.Unlock()
		return
	}
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
	e.cancel()
	resourceErr := e.resources.close()
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
		return resourceErr
	}
	if err := bridge.Close(); err != nil {
		return err
	}
	return resourceErr
}

func (e *Engine) ExecuteModule(module *LoadedModule, main bool) error {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	err := e.executeModuleLocked(module, main)
	if main {
		if err != nil {
			e.cancel()
		}
		for i := 0; i < len(e.tasks); i++ {
			task := e.tasks[i]
			e.execMu.Unlock()
			<-task.done
			e.execMu.Lock()
			if err == nil && task.result.err != nil && !(task.cancelled && errors.Is(task.result.err, context.Canceled)) {
				err = fmt.Errorf("child task: %w", task.result.err)
				e.cancel()
			}
		}
	}
	return err
}

func (e *Engine) executeModuleLocked(module *LoadedModule, main bool) error {
	if module.Executing {
		return fmt.Errorf("cyclic module execution: %s", module.Path)
	}
	if module.ExecuteErr != nil {
		return module.ExecuteErr
	}
	if module.Executed {
		return nil
	}
	module.Globals["__file__"] = StringValue(module.Path)
	if main {
		module.Globals["__name__"] = StringValue("__main__")
	} else {
		module.Globals["__name__"] = StringValue(module.Name)
	}
	module.Executing = true
	defer func() { module.Executing = false }()
	vm := &VM{engine: e, module: module, depth: 1}
	_, err := vm.runFunction(module.Program.Module, nil)
	if err != nil {
		module.ExecuteErr = err
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
	} else if !os.IsNotExist(err) {
		return "", err
	}
	initPath := filepath.Join(base, "__init__.ty")
	if _, err := os.Stat(initPath); err == nil {
		return initPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return "", fmt.Errorf("%w: cannot import module %q from %s", ErrModuleNotFound, module, fromDir)
}

var ErrModuleNotFound = errors.New("Typhon module not found")

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
			"spawn":      {Kind: TypeFunc, Name: "spawn", Variadic: true, Params: []Type{AnyType()}, Return: typePtr(TaskType())},
			"join":       {Kind: TypeFunc, Name: "join", Params: []Type{TaskType()}, Return: typePtr(AnyType())},
			"cancel":     {Kind: TypeFunc, Name: "cancel", Params: []Type{TaskType()}, Return: typePtr(NoneType())},
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
