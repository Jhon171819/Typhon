package typhon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type repeatedHTTPByte byte

func (b repeatedHTTPByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

const uploadStreamFixture = `import typhon.http as http
import typhon.scalars as s
import typhon.web as web

routes: web.Router = web.router()
routes.use_errors()

def upload(req: http.Request) -> http.Response:
    form: http.Multipart = req.multipart()
    total: int = 0
    while True:
        part: http.Part | None = form.next()
        if part is None:
            break
        while True:
            chunk: s.Bytes | None = part.read(65536)
            if chunk is None:
                break
            total = total + chunk.size
        part.close()
    form.close()
    return http.json(200, total)

def raw(req: http.Request) -> http.Response:
    body: http.Body = req.stream()
    total: int = 0
    while True:
        chunk: s.Bytes | None = body.read(65536)
        if chunk is None:
            break
        total = total + chunk.size
    body.close()
    return http.json(200, total)

routes.add('POST', '/upload', upload)
routes.stream_upload('POST', '/upload', 16777216, 12582912, 16)
routes.add('POST', '/raw', raw)
routes.stream_upload('POST', '/raw', 50331648)
`

func streamingFixture(t testing.TB, source string) http.Handler {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	path, err := os.CreateTemp(dir, "*.ty")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(path, source); err != nil {
		t.Fatal(err)
	}
	if err := path.Close(); err != nil {
		t.Fatal(err)
	}
	module, err := engine.LoadModule(path.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	return httpVMHandler(engine, module.Globals["routes"])
}

func TestNativeStreamingMultipartUpload(t *testing.T) {
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	t.Setenv("TMP", temporary)
	t.Setenv("TEMP", temporary)
	server := httptest.NewServer(streamingFixture(t, uploadStreamFixture))
	defer server.Close()
	for _, tc := range []struct {
		name, filename string
		payload        int64
		parts, status  int
	}{
		{"larger_than_8MiB", "upload.bin", 10 << 20, 1, 200},
		{"traversal", "../escape.bin", 32, 1, 400},
		{"windows_path", `C:\private\file.bin`, 32, 1, 400},
		{"per_part_limit", "large.bin", 13 << 20, 1, 413},
		{"100_parts", "part.bin", 1, 100, 413},
		{"connection_limit", "large.bin", 9 << 20, 2, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pr.Close()
			form := multipart.NewWriter(pw)
			written := make(chan error, 1)
			go func() {
				write := func() error {
					for range tc.parts {
						var part io.Writer
						if strings.Contains(tc.filename, "\\") {
							header := make(textproto.MIMEHeader)
							header.Set("Content-Disposition", `form-data; name="file"; filename="C:\\private\\file.bin"`)
							part, _ = form.CreatePart(header)
						} else {
							var err error
							part, err = form.CreateFormFile("file", tc.filename)
							if err != nil {
								return err
							}
						}
						if part == nil {
							return io.ErrClosedPipe
						}
						if _, err := io.CopyN(part, repeatedHTTPByte(0xc3), tc.payload); err != nil {
							return err
						}
					}
					return form.Close()
				}
				err := write()
				_ = pw.CloseWithError(err)
				written <- err
			}()
			req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/upload", pr)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", form.FormDataContentType())
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			_ = pr.Close()
			sendErr := <-written
			if err != nil || resp.StatusCode != tc.status || tc.status == 200 && (string(body) != fmt.Sprint(tc.payload) || sendErr != nil) {
				t.Fatalf("upload: HTTP %d %s, read %v, send %v", resp.StatusCode, body, err, sendErr)
			}
		})
	}
	entries, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "multipart-") {
			t.Fatalf("multipart left temporary upload %q", entry.Name())
		}
	}
}

func TestNativeStreamingDownload(t *testing.T) {
	source := fmt.Sprintf(`import typhon.http as http
import typhon.scalars as s
import typhon.web as web

chunk: s.Bytes = s.bytes_utf8(%q)
routes: web.Router = web.router()
routes.use_errors()

def produce(state: Any) -> s.Bytes | None:
    sent: list[int] = state
    if sent[0] == 640:
        return None
    sent[0] = sent[0] + 1
    return chunk

def fail(state: Any) -> s.Bytes | None:
    sent: list[int] = state
    if sent[0] == 1:
        raise Error('export stopped')
    sent[0] = sent[0] + 1
    return chunk

def before_headers(state: Any) -> s.Bytes | None:
    raise ValidationError('export unavailable')

def export(req: http.Request) -> http.Response:
    sent: list[int] = [0]
    return http.stream(200, produce, sent, [['Content-Type', 'text/csv']])

def broken(req: http.Request) -> http.Response:
    sent: list[int] = [0]
    return http.stream(200, fail, sent)

def unstarted(req: http.Request) -> http.Response:
    return http.stream(200, before_headers, None)

routes.add('GET', '/export', export)
routes.add('GET', '/broken', broken)
routes.add('GET', '/unstarted', unstarted)
`, strings.Repeat("a", 16<<10))
	server := httptest.NewServer(streamingFixture(t, source))
	defer server.Close()
	for _, tc := range []struct {
		method, path string
		status       int
		size         int64
		truncated    bool
	}{
		{"GET", "/export", 200, 10 << 20, false},
		{"HEAD", "/export", 200, 0, false},
		{"GET", "/broken", 200, 16 << 10, true},
		{"GET", "/unstarted", 422, -1, false},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, server.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		count, err := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || tc.size >= 0 && count != tc.size || tc.truncated != (err != nil) {
			t.Fatalf("%s %s -> %d (%d bytes, %v)", tc.method, tc.path, resp.StatusCode, count, err)
		}
		if tc.path == "/export" && resp.Header.Get("Content-Type") != "text/csv" {
			t.Fatal("CSV response lost its media type")
		}
	}
}

func testHTTPStreamCancellation(t *testing.T, resource func(*VM) (func(context.Context) error, error)) {
	t.Helper()
	engine := NewEngine()
	defer engine.Close()
	var closed atomic.Int32
	streamEntered, finished := make(chan struct{}), make(chan struct{})
	handler := driverFunc(httpHandlerType(), func(vm *VM, _ []Value) (Value, error) {
		closeResource, err := resource(vm)
		if err != nil {
			return NoneValue(), err
		}
		if err := vm.resourceScope().add(vm.context(), finished, "stream reader", func(ctx context.Context) error {
			closed.Add(1)
			defer close(finished)
			return closeResource(ctx)
		}); err != nil {
			return NoneValue(), err
		}
		sent := false
		producer := driverFunc(httpDriverTypes()["stream"].Params[1], func(vm *VM, args []Value) (Value, error) {
			if !sent {
				sent = true
				return nativeScalarValue(vm, "Bytes", bytes.Repeat([]byte("csv\n"), 1024))
			}
			close(streamEntered)
			<-vm.context().Done()
			return NoneValue(), vm.context().Err()
		})
		return makeHTTPStream(vm, []Value{IntValue(200), producer, NoneValue()})
	})
	server := httptest.NewServer(httpVMHandler(engine, handler))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 32)
	_, err = io.ReadFull(resp.Body, first)
	if err != nil || !bytes.HasPrefix(first, []byte("csv\n")) {
		resp.Body.Close()
		t.Fatalf("first buffered chunk was missing: %v", err)
	}
	select {
	case <-streamEntered:
	case <-ctx.Done():
		resp.Body.Close()
		t.Fatal("stream did not stay active")
	}
	_ = resp.Body.Close()
	select {
	case <-finished:
		if closed.Load() != 1 {
			t.Fatal("request did not release the cursor exactly once")
		}
	case <-time.After(time.Second):
		t.Fatal("download cancellation left its resource open")
	}
}

func TestNativeStreamingDownloadCancellation(t *testing.T) {
	testHTTPStreamCancellation(t, func(*VM) (func(context.Context) error, error) {
		return func(context.Context) error { return nil }, nil
	})
}

func TestNativeStreamingCursorCancellationIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_POSTGRES_URL to verify cursor release after disconnect")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", validationKind, err))
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(driverFailure("PostgreSQL", connectionKind, err))
	}
	defer pool.Close()
	testHTTPStreamCancellation(t, func(vm *VM) (func(context.Context) error, error) {
		rows, err := pool.Query(vm.context(), "SELECT id FROM generate_series(1, 100000) AS id")
		if err != nil {
			return nil, driverFailure("PostgreSQL", databaseKind, err)
		}
		if !rows.Next() {
			rows.Close()
			return nil, fmt.Errorf("cursor had no first row: %w", driverFailure("PostgreSQL", databaseKind, rows.Err()))
		}
		return func(context.Context) error { rows.Close(); return nil }, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var marker int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("cursor did not return the only connection: %v", driverFailure("PostgreSQL", databaseKind, err))
	}
}

type peakUploadReader struct {
	read, sample int64
	peak         uint64
}

func (r *peakUploadReader) Read(p []byte) (int, error) {
	if r.read >= r.sample {
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		r.peak = max(r.peak, mem.HeapAlloc)
		r.sample += 1 << 20
	}
	n, err := repeatedHTTPByte('x').Read(p)
	r.read += int64(n)
	return n, err
}

func BenchmarkNativeHTTPStreamingLiveHeap(b *testing.B) {
	for _, size := range []int64{4 << 20, 16 << 20, 40 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			handler := streamingFixture(b, uploadStreamFixture)
			b.SetBytes(size)
			b.ResetTimer()
			var peak uint64
			for range b.N {
				b.StopTimer()
				runtime.GC()
				var baseline runtime.MemStats
				runtime.ReadMemStats(&baseline)
				source := &peakUploadReader{sample: 1 << 20, peak: baseline.HeapAlloc}
				req := httptest.NewRequest("POST", "/raw", io.LimitReader(source, size))
				req.ContentLength = size
				rec := httptest.NewRecorder()
				b.StartTimer()
				handler.ServeHTTP(rec, req)
				b.StopTimer()
				if rec.Code != 200 || rec.Body.String() != fmt.Sprint(size) {
					b.Fatalf("stream failed: %d %s", rec.Code, rec.Body)
				}
				peak = max(peak, source.peak-baseline.HeapAlloc)
			}
			b.ReportMetric(float64(peak), "peak-live-B")
		})
	}
}
