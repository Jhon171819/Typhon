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
	arrived := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(arrived)
		<-req.Context().Done()
	}))
	defer upstream.Close()
	var setup, insert string
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
%s    upstream.get(%q)
    return http.response(200, 'done')
try:
    http.serve(%q, handler)
finally:
    upstream.close()
    print('cleanup ran')
`, setup, insert, upstream.URL, address)
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSIGTERMShutsDownActiveRequest$")
	child.Env = append(os.Environ(), "TYPHON_TEST_SIGNAL_CHILD="+file)
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
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		if response, err := client.Get(url + "/active"); err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("active request did not reach the upstream")
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil || !strings.Contains(output.String(), "cleanup ran") {
		t.Fatalf("shutdown: %v, %s", err, output.String())
	}
	<-requestDone
	if admin != nil {
		var rows int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("SIGTERM left %d rows after rollback", rows)
		}
	}
}
