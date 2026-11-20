package typhon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func moduleFixture(t *testing.T, root, name, source string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalModuleAliasesAndEngineIsolation(t *testing.T) {
	root := t.TempDir()
	moduleFixture(t, root, "pkg/__init__.ty", `type Label = str
state: list[int] = []
def visit() -> int:
    state.append(1)
    return len(state)
class Profile:
    name: str
    def __init__(self, name: str) -> None:
        self.name = name
`)
	moduleFixture(t, root, "other.ty", "value: int = 2\n")
	path := moduleFixture(t, root, "main.ty", `import pkg as package
from pkg import Profile as Person, Label as Name, visit as touch
from other import value as sibling
user: package.Profile = Person('Ada')
second: Person = package.Profile('Grace')
name: Name = user.name + second.name
first: int = touch()
next: int = package.visit()
`)
	if err := CheckImports(path); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		engine := NewEngine()
		module, err := engine.LoadModule(path)
		if err == nil {
			err = engine.ExecuteModule(module, true)
		}
		if err != nil {
			engine.Close()
			t.Fatal(err)
		}
		if engine.bridge != nil || len(engine.modules) != 3 ||
			module.Globals["first"].Int != 1 || module.Globals["next"].Int != 2 ||
			module.Globals["sibling"].Int != 2 || module.Globals["name"].Str != "AdaGrace" {
			t.Error("module initialized twice, escaped its Engine, or lost an import alias")
		}
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModuleResolutionErrors(t *testing.T) {
	root := t.TempDir()
	moduleFixture(t, root, "a.ty", "import b\n")
	moduleFixture(t, root, "b.ty", "import a\n")
	moduleFixture(t, root, "present.ty", "x: int = 1\n")
	for _, tc := range []struct{ name, source, want string }{
		{"cycle.ty", "import a\n", "cyclic Typhon import"},
		{"missing.ty", "import unavailable as alias\n", "cannot import module"},
		{"symbol.ty", "from present import nope as alias\n", "has no export"},
		{"traversal.ty", "from ../present import x\n", "invalid import module"},
		{"badalias.ty", "import present as bad-name\n", "optional alias"},
		{"type.ty", "import present as p\nvalue: str = p.x\n", "expected str"},
	} {
		path := moduleFixture(t, root, tc.name, tc.source)
		err := CheckImports(path)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.name+":") {
			t.Errorf("%s: expected %q with source location, got %v", tc.name, tc.want, err)
		}
	}
	if err := CheckImports(moduleFixture(t, root, "python.ty", "from py.math import sqrt as root\n")); err != nil {
		t.Fatalf("explicit Python import should be allowed: %v", err)
	}
}

func TestModuleRootAndSymlinkContainment(t *testing.T) {
	root := t.TempDir()
	moduleFixture(t, root, "config.ty", "value: int = 1\n")
	entry := moduleFixture(t, root, "app/main.ty", "from config import value\n")
	if err := CheckImports(entry); err == nil || !strings.Contains(err.Error(), "cannot import module") {
		t.Fatalf("implicit module root escaped to parent: %v", err)
	}
	t.Setenv("TYPHON_MODULE_ROOT", root)
	if err := CheckImports(entry); err != nil {
		t.Fatal(err)
	}
	outside := moduleFixture(t, t.TempDir(), "outside.ty", "value: int = 2\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape.ty")); err != nil {
		t.Skipf("symlink capability unavailable: %v", err)
	}
	escape := moduleFixture(t, root, "escape_main.ty", "import escape as escaped\n")
	if err := CheckFile(escape); err == nil || !strings.Contains(err.Error(), "escape_main.ty:1") {
		t.Fatalf("symlink escaped the configured root: %v", err)
	}
}

func TestNativeModularHTTPExample(t *testing.T) {
	engine := NewEngine()
	defer engine.Close()
	path := filepath.Join(repoRoot(t), "examples", "modular_app", "main.ty")
	if err := CheckImports(path); err != nil {
		t.Fatal(err)
	}
	module, err := engine.LoadModule(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	route := module.Globals["routes"].Module.Loaded
	handler := httpVMHandler(engine, route.Globals["handle"])
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?name=Ada", nil))
	if w.Code != 200 || w.Body.String() != `{"version": "v1", "name": "Ada"}` || engine.bridge != nil || len(engine.modules) != 6 {
		t.Fatalf("modular HTTP execution: %d %s, %d modules", w.Code, w.Body, len(engine.modules))
	}
}
