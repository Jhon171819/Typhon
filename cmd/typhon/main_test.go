package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSharedPythonFlagOnlyAppliesToRun(t *testing.T) {
	err := run([]string{"--shared-python", "check", "unused.ty"})
	if err == nil || !strings.Contains(err.Error(), "only apply to run") {
		t.Fatalf("expected option scope error, got %v", err)
	}
}

func TestSharedPythonEnvironmentDoesNotBlockCheck(t *testing.T) {
	t.Setenv("TYPHON_SHARED_PYTHON", "1")
	path := filepath.Join("..", "..", "examples", "void_return.ty")
	if err := run([]string{"check", path}); err != nil {
		t.Fatal(err)
	}
}

func TestSIGTERMShutsDownActiveRequest(t *testing.T) {
	if file := os.Getenv("TYPHON_TEST_SIGNAL_CHILD"); file != "" {
		os.Args = []string{"typhon", "run", file}
		main()
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX SIGTERM delivery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	arrived := map[string]chan struct{}{
		"/short": make(chan struct{}), "/long": make(chan struct{}), "/commit": make(chan struct{}),
	}
	release := make(chan struct{})
	releaseWaiters := sync.OnceFunc(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(arrived[req.URL.Path])
		if req.URL.Path == "/long" {
			<-req.Context().Done()
			return
		}
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
		}
	}))
	defer upstream.Close()
	defer releaseWaiters()
	var setup, insert, commitBegin, commitEnd string
	var admin *pgx.Conn
	var table string
	if dsn := os.Getenv("TYPHON_TEST_POSTGRES_URL"); dsn != "" {
		var err error
		admin, err = pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal("could not connect to the dedicated PostgreSQL fixture")
		}
		table = pgx.Identifier{fmt.Sprintf("typhon_sigterm_%d", time.Now().UnixNano())}.Sanitize()
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			if _, err := admin.Exec(cleanup, "DROP TABLE IF EXISTS "+table); err != nil {
				t.Error("could not remove the PostgreSQL fixture")
			}
			if err := admin.Close(cleanup); err != nil {
				t.Error("could not close the PostgreSQL fixture")
			}
		})
		if _, err := admin.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
			t.Fatal("could not create the PostgreSQL fixture")
		}
		setup = fmt.Sprintf("import typhon.postgres as pg\ndb: pg.Connection = pg.connect(%q)\n", dsn)
		insert = fmt.Sprintf("    tx: pg.Transaction = db.begin()\n    tx.exec(%q, [1])\n", "INSERT INTO "+table+" (id) VALUES ($1)")
		commitBegin = fmt.Sprintf("        finished: pg.Transaction = db.begin()\n        finished.exec(%q, [2])\n", "INSERT INTO "+table+" (id) VALUES ($1)")
		commitEnd = "        finished.commit()\n"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	file := filepath.Join(t.TempDir(), "signal.ty")
	source := fmt.Sprintf(`import typhon.http as http
%supstream: http.Client = http.client()
def handler(req: http.Request) -> http.Response:
    if req.path == '/health':
        return http.response(200, 'ready')
    if req.path == '/short':
        upstream.get(%q)
        return http.response(200, 'short completed')
    if req.path == '/commit':
%s        upstream.get(%q)
%s        return http.response(201, 'commit completed')
%s    upstream.get(%q)
    return http.response(200, 'done')
try:
    http.serve(%q, handler, 3, 4)
finally:
    upstream.close()
    print('cleanup ran')
`, setup, upstream.URL+"/short", commitBegin, upstream.URL+"/commit", commitEnd, insert, upstream.URL+"/long", address)
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSIGTERMShutsDownActiveRequest$")
	child.Env = append(os.Environ(), "TYPHON_TEST_SIGNAL_CHILD="+file,
		"TYPHON_HTTP_SHUTDOWN_GRACE=1s", "TYPHON_READINESS_PATH=/ready")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			cancel()
			_ = child.Wait()
		}
	}()
	client := &http.Client{Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	url := "http://" + address
	deadline := time.Now().Add(4 * time.Second)
	for {
		response, err := client.Get(url + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI server did not become available")
		}
		time.Sleep(10 * time.Millisecond)
	}
	request := func(method, path string) <-chan int {
		done := make(chan int, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, method, url+path, nil)
			if err != nil {
				done <- 0
				return
			}
			response, err := client.Do(req)
			if err != nil {
				done <- 0
				return
			}
			response.Body.Close()
			done <- response.StatusCode
		}()
		return done
	}
	short := request(http.MethodGet, "/short")
	long := request(http.MethodPost, "/long")
	commit := request(http.MethodPost, "/commit")
	for path, seen := range arrived {
		select {
		case <-seen:
		case <-ctx.Done():
			t.Fatalf("active request %s did not reach the upstream", path)
		}
	}
	signalled := time.Now()
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for {
		probe := <-request(http.MethodGet, "/ready")
		if probe == http.StatusServiceUnavailable {
			break
		}
		if probe != http.StatusOK || time.Since(signalled) >= 800*time.Millisecond {
			t.Fatalf("readiness did not switch during drain: %d", probe)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := <-request(http.MethodGet, "/health"); got != http.StatusServiceUnavailable {
		t.Fatalf("server admitted new work after beginning drain: %d", got)
	}
	releaseWaiters()
	if got := <-short; got != http.StatusOK {
		t.Fatalf("short GET was interrupted during grace: %d", got)
	}
	if got := <-commit; got != http.StatusCreated {
		t.Fatalf("short transaction was interrupted during grace: %d", got)
	}
	if err := child.Wait(); err != nil || !strings.Contains(output.String(), "cleanup ran") {
		t.Fatalf("shutdown: %v, %s", err, output.String())
	}
	if elapsed := time.Since(signalled); elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("shutdown did not honor the 1s grace period: %s", elapsed)
	}
	if got := <-long; got != http.StatusServiceUnavailable {
		t.Fatalf("long request was not cancelled when grace expired: %d", got)
	}
	if admin != nil {
		var rows, last int
		if err := admin.QueryRow(ctx, "SELECT count(*), coalesce(max(id),0) FROM "+table).Scan(&rows, &last); err != nil || rows != 1 || last != 2 {
			t.Fatalf("SIGTERM expected only committed row 2, got rows=%d last=%d err=%v", rows, last, err)
		}
	}
}
