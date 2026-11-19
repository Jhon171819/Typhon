package typhon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkHTTPJSONReference(b *testing.B) {
	handlers := []struct {
		name string
		make func(*testing.B) http.Handler
	}{
		{"Typhon", benchmarkTyphonJSON},
		{"Go", func(_ *testing.B) http.Handler {
			type item struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			payload := struct {
				Items []item `json:"items"`
			}{[]item{{"one", "Ada"}, {"two", "Grace"}}}
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_ = json.NewEncoder(w).Encode(payload)
			})
		}},
	}
	for _, item := range handlers {
		b.Run(item.name, func(b *testing.B) {
			server := httptest.NewServer(item.make(b))
			b.Cleanup(server.Close)
			client := server.Client()
			b.ReportAllocs()
			for b.Loop() {
				response, err := client.Get(server.URL + "/sample")
				if err != nil {
					b.Fatal(err)
				}
				_, err = io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if err != nil || closeErr != nil || response.StatusCode != 200 {
					b.Fatalf("response status=%d read=%v close=%v", response.StatusCode, err, closeErr)
				}
			}
		})
	}
}

func benchmarkTyphonJSON(b *testing.B) http.Handler {
	engine, module := loadBenchmarkModule(b, `
import typhon.http as http

def sample(req: http.Request) -> http.Response:
    return http.json(200, {'items': [{'id': 'one', 'name': 'Ada'}, {'id': 'two', 'name': 'Grace'}]})
`)
	return httpVMHandler(engine, module.Globals["sample"])
}

func BenchmarkWarmedTypedIntegerLoop(b *testing.B) {
	engine, module := loadBenchmarkModule(b, `
def sum_values(values: list[int]) -> int:
    total: int = 0
    for value: int in values:
        total = total + value
    return total
`)
	function := module.Globals["sum_values"]
	args := []Value{ListValue([]Value{IntValue(1), IntValue(2), IntValue(3), IntValue(4), IntValue(5)})}
	b.ReportAllocs()
	for b.Loop() {
		vm := VM{engine: engine, module: module}
		result, err := vm.callValue(function, args, nil)
		if err != nil || result.Int != 15 {
			b.Fatalf("sum: %v, error: %v", result, err)
		}
	}
}

func loadBenchmarkModule(b *testing.B, source string) (*Engine, *LoadedModule) {
	b.Helper()
	file := filepath.Join(b.TempDir(), "benchmark.ty")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		b.Fatal(err)
	}
	engine := NewEngine()
	b.Cleanup(func() {
		if err := engine.Close(); err != nil {
			b.Error(err)
		}
	})
	module, err := engine.LoadModule(file)
	if err != nil {
		b.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		b.Fatal(err)
	}
	return engine, module
}
