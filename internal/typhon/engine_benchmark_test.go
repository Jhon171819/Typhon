package typhon

import (
	"os"
	"path/filepath"
	"testing"
)

func benchmarkRunDiscardOutput(b *testing.B, path string) {
	b.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer devNull.Close()
	oldStdout := os.Stdout
	os.Stdout = devNull
	defer func() {
		os.Stdout = oldStdout
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := RunFile(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStartupVoidReturn(b *testing.B) {
	benchmarkRunDiscardOutput(b, filepath.Join("..", "..", "examples", "void_return.ty"))
}

func BenchmarkTypedIntegerLoop(b *testing.B) {
	path := filepath.Join(b.TempDir(), "loop.ty")
	source := `
def sum_values(values: list[int]) -> int:
    total: int = 0
    for value: int in values:
        total = total + value
    return total

print(sum_values([1, 2, 3, 4, 5]))
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		b.Fatal(err)
	}
	benchmarkRunDiscardOutput(b, path)
}

func BenchmarkFunctionCallAndClassFields(b *testing.B) {
	benchmarkRunDiscardOutput(b, filepath.Join("..", "..", "examples", "user_service.ty"))
}

func BenchmarkParallelTask(b *testing.B) {
	benchmarkRunDiscardOutput(b, filepath.Join("..", "..", "examples", "parallel_task.ty"))
}

func BenchmarkBytecodeCompileUserService(b *testing.B) {
	path := filepath.Join("..", "..", "examples", "user_service.ty")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BytecodeFile(path); err != nil {
			b.Fatal(err)
		}
	}
}
