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
	b.Helper()
	file := filepath.Join(b.TempDir(), "sample.ty")
	if err := os.WriteFile(file, []byte(`
import typhon.http as http

def sample(req: http.Request) -> http.Response:
    return http.json(200, {'items': [{'id': 'one', 'name': 'Ada'}, {'id': 'two', 'name': 'Grace'}]})
`), 0600); err != nil {
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
	return httpVMHandler(engine, module.Globals["sample"])
}
