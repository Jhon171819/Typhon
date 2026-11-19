package typhon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (e *Engine) initializeModuleRoot(dir string) error {
	if e.moduleRoot != "" {
		return nil
	}
	if configured := os.Getenv("TYPHON_MODULE_ROOT"); configured != "" {
		dir = configured
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	directory, err := os.Stat(root) // #nosec G703 -- The Go host selects this import root. Dynamic imports use os.Root below.
	if err != nil {
		return err
	}
	if !directory.IsDir() {
		return fmt.Errorf("Typhon module root is not a directory: %s", root)
	}
	e.moduleRoot = root
	return nil
}

// modulePath resolves rooted, dotted Typhon names. The caller holds moduleMu.
func (e *Engine) modulePath(module, fromDir string) (string, error) {
	if !validModuleName(module) {
		return "", fmt.Errorf("invalid import module name %q", module)
	}
	if err := e.initializeModuleRoot(fromDir); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(e.moduleRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	bases := []string{"."}
	absFrom, err := filepath.Abs(fromDir)
	if err != nil {
		return "", err
	}
	if current, err := filepath.Rel(e.moduleRoot, absFrom); err == nil && filepath.IsLocal(current) && current != "." {
		bases = []string{current, "."}
	}
	local := filepath.Join(strings.Split(module, ".")...)
	for _, base := range bases {
		name := filepath.Join(base, local)
		for _, candidate := range []string{name + ".ty", filepath.Join(name, "__init__.ty")} {
			info, err := root.Stat(candidate)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("cannot resolve module %q at %s: %w", module, candidate, err)
			}
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("module %q is not a regular file: %s", module, candidate)
			}
			return filepath.Join(e.moduleRoot, candidate), nil
		}
	}
	return "", fmt.Errorf("%w: cannot import module %q from %s (root %s)", ErrModuleNotFound, module, fromDir, e.moduleRoot)
}

func (e *Engine) readImportedSource(path string) ([]byte, error) {
	rel, err := filepath.Rel(e.moduleRoot, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("Typhon import leaves module root: %s", path)
	}
	root, err := os.OpenRoot(e.moduleRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(rel)
}
