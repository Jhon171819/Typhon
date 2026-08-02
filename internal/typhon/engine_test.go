package typhon

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func examplePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", name)
}

func captureRun(t *testing.T, path string) (string, error) {
	t.Helper()
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	runErr := RunFile(path)
	_ = writer.Close()
	os.Stdout = oldStdout
	output, readErr := io.ReadAll(reader)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return strings.ReplaceAll(string(output), "\r\n", "\n"), runErr
}

func TestRunUserServiceExample(t *testing.T) {
	output, err := captureRun(t, examplePath(t, "user_service.ty"))
	if err != nil {
		t.Fatal(err)
	}
	want := "Ada Lovelace\n1: Ada Lovelace\n2: Grace Hopper\n"
	if output != want {
		t.Fatalf("unexpected output:\nwant %q\ngot  %q", want, output)
	}
}

func TestRunMainScriptImportsTyphonModule(t *testing.T) {
	output, err := captureRun(t, examplePath(t, "main_script.ty"))
	if err != nil {
		t.Fatal(err)
	}
	want := "__main__\nlocal import works: typhon\n"
	if output != want {
		t.Fatalf("unexpected output:\nwant %q\ngot  %q", want, output)
	}
}

func TestRunParallelTaskExample(t *testing.T) {
	output, err := captureRun(t, examplePath(t, "parallel_task.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if output != "42\n" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestRunPythonBridgeExample(t *testing.T) {
	output, err := captureRun(t, examplePath(t, "python_bridge.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if output != "[4, 2]\n" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestRunPlainPythonImportExample(t *testing.T) {
	output, err := captureRun(t, examplePath(t, "python_plain_import.ty"))
	if err != nil {
		t.Fatal(err)
	}
	if output != "[1, 2]\n" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestRunSingleQuotedStringLiteral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "single_quote.ty")
	source := "print('single quotes work')\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if output != "single quotes work\n" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestRunPythonLikeLoopKeywordAndFormattingFeatures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pythonish.ty")
	source := `
import py.datetime as dt


def main() -> void:
    items: list[str] = ['a', 'b']

    for index: int, item: str in enumerate(items):
        start_time: float = 1.25
        end_time: float = 3.5
        elapsed_ms: float = (end_time - start_time) * 1000
        print(f"{index + 1}: {item} {elapsed_ms:.2f}")

    value: PyObject = dt.datetime(year=2020, month=1, day=2)
    print(value)


main()
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1: a 2250.00",
		"2: b 2250.00",
		"datetime.datetime(2020, 1, 2, 0, 0)",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunRangeLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "range_loop.ty")
	source := `
for item: int in range(5):
    print(item)

for item: int in range(2, 7, 2):
    print(item)
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, path)
	if err != nil {
		t.Fatal(err)
	}
	want := "0\n1\n2\n3\n4\n2\n4\n6\n"
	if output != want {
		t.Fatalf("unexpected output:\nwant %q\ngot  %q", want, output)
	}
}

func TestCheckRejectsInvalidCall(t *testing.T) {
	err := CheckFile(examplePath(t, "runtime_type_error.ty"))
	if err == nil {
		t.Fatal("expected type error")
	}
	if !strings.Contains(err.Error(), "argument 1 expected int, got str") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBytecodeCommandIncludesFunctionInstructions(t *testing.T) {
	text, err := BytecodeFile(examplePath(t, "void_return.ty"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"function <module>", "DEFINE_FUNC", "CALL"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bytecode missing %q:\n%s", want, text)
		}
	}
}

func TestParserHandlesMultilineFunctionAndList(t *testing.T) {
	source, err := os.ReadFile(examplePath(t, "user_service.ty"))
	if err != nil {
		t.Fatal(err)
	}
	module, err := ParseFile(examplePath(t, "user_service.ty"), string(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(module.Stmts) == 0 {
		t.Fatal("expected parsed statements")
	}
}
