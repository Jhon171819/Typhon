package typhon

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func streamTestCall(t *testing.T, vm *VM, fn Value, args ...Value) Value {
	t.Helper()
	value, err := vm.callValue(fn, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func streamTestEngine(t *testing.T) *VM {
	t.Helper()
	engine := NewEngine()
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	return &VM{engine: engine}
}

func streamTestRead(t *testing.T, vm *VM, stream Value, consumer string) Value {
	t.Helper()
	messages := streamTestCall(t, vm, stream.NativeObject.Attrs["read"], StringValue(consumer), IntValue(10))
	if len(*messages.List) != 1 {
		t.Fatalf("expected one new delivery, got %d", len(*messages.List))
	}
	return (*messages.List)[0]
}

func streamTestClaim(t *testing.T, vm *VM, stream Value, consumer string, attempt int64) Value {
	t.Helper()
	time.Sleep(10 * time.Millisecond)
	batch := streamTestCall(t, vm, stream.NativeObject.Attrs["claim"], StringValue(consumer), IntValue(1), StringValue("0-0"), IntValue(10))
	messages := batch.NativeObject.Attrs["messages"]
	if len(*messages.List) != 1 {
		t.Fatalf("expected one reclaimed delivery, got %d", len(*messages.List))
	}
	delivery := (*messages.List)[0]
	if got := delivery.NativeObject.Attrs["attempt"].Int; got != attempt {
		t.Fatalf("delivery attempt: got %d, expected %d", got, attempt)
	}
	return delivery
}

func TestNativeRedisStreamOwnershipAndCancellation(t *testing.T) {
	server := miniredis.RunT(t)
	vm := streamTestEngine(t)
	connection, err := nativeRedisConnect(vm, []Value{StringValue("redis://" + server.Addr())})
	if err != nil {
		t.Fatal(err)
	}
	stream := streamTestCall(t, vm, connection.NativeObject.Attrs["stream"], StringValue("reports"), StringValue("workers"), IntValue(2))
	attrs := stream.NativeObject.Attrs
	streamTestCall(t, vm, attrs["create_group"])
	streamTestCall(t, vm, attrs["create_group"])
	entry := streamTestCall(t, vm, attrs["publish"], IntValue(123))
	first := streamTestRead(t, vm, stream, "old")
	if first.NativeObject.Attrs["id"].Str != entry.Str || first.NativeObject.Attrs["attempt"].Int != 1 {
		t.Fatal("initial delivery identifier or attempt was lost")
	}

	owned := streamTestClaim(t, vm, stream, "new", 2)
	if streamTestCall(t, vm, first.NativeObject.Attrs["ack"]).Bool {
		t.Fatal("stale delivery acknowledged the reclaimed message")
	}
	newer := streamTestClaim(t, vm, stream, "new", 3)
	if streamTestCall(t, vm, owned.NativeObject.Attrs["dead_letter"], StringValue("stale")).Bool {
		t.Fatal("older delivery attempt moved the message")
	}
	if !streamTestCall(t, vm, newer.NativeObject.Attrs["dead_letter"], StringValue("test")).Bool ||
		streamTestCall(t, vm, newer.NativeObject.Attrs["ack"]).Bool {
		t.Fatal("dead-letter transition did not acknowledge exactly once")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	vm.ctx = ctx
	start := time.Now()
	_, err = vm.callValue(attrs["read"], []Value{StringValue("new"), IntValue(1), IntValue(500)}, nil)
	if !errors.Is(err, context.Canceled) || time.Since(start) >= 400*time.Millisecond {
		t.Fatalf("blocking Redis read survived its context: %v, elapsed %s", err, time.Since(start))
	}
	vm.ctx = nil
	for _, value := range []int64{1, 2, 3} {
		streamTestCall(t, vm, attrs["publish"], IntValue(value))
	}
	raw := connection.NativeObject.data.(*redisConnection).client
	size, err := raw.XLen(t.Context(), "typhon:stream:{reports}").Result()
	if err != nil || size != 2 {
		t.Fatalf("bounded stream length: %d: %v", size, err)
	}
}

func streamExampleModule(t *testing.T, name string) (*VM, *LoadedModule) {
	t.Helper()
	vm := streamTestEngine(t)
	path := filepath.Join("..", "..", "examples", "streams", name+".ty")
	module, err := vm.engine.LoadModule(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.engine.ExecuteModule(module, false); err != nil {
		t.Fatal(err)
	}
	return vm, module
}

func TestNativeRedisStreamWorkerIntegration(t *testing.T) {
	redisURL := os.Getenv("TYPHON_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL to exercise Redis Streams")
	}
	ctx, dsn, admin, schema := nativeMigrationDB(t)
	if err := RunMigrations(ctx, dsn, filepath.Join("..", "..", "examples", "streams", "migrations"), "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "ALTER TABLE "+schema+".report_results ADD CHECK (title <> 'unavailable')"); err != nil {
		t.Fatal(err)
	}
	streamName := "report-" + rand.Text()
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("REDIS_URL", redisURL)
	t.Setenv("REPORT_STREAM", streamName)
	producer, producerModule := streamExampleModule(t, "producer")
	handler := httpVMHandler(producer.engine, producerModule.Globals["routes"])
	send := func(body string, want int) {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/reports", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		got := httptest.NewRecorder()
		handler.ServeHTTP(got, req)
		if got.Code != want {
			t.Fatalf("producer got %d %s, expected %d", got.Code, got.Body.String(), want)
		}
	}
	countResults := func(want int64) {
		t.Helper()
		var count int64
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".report_results").Scan(&count); err != nil || count != want {
			t.Fatalf("expected %d persisted results, got %d: %v", want, count, err)
		}
	}
	send(`{"version":2,"kind":"report","job_id":"abc","title":"report"}`, 422)
	send(`{"version":1,"kind":"report","job_id":"abc","title":"report"}`, 202)
	countResults(0)
	first, worker := streamExampleModule(t, "worker")
	delivery := streamTestRead(t, first, worker.Globals["jobs"], "worker-a")
	payload := streamTestCall(t, first, delivery.NativeObject.Attrs["decode"])
	streamTestCall(t, first, worker.Globals["persist"], payload)
	countResults(1)
	if err := first.engine.Close(); err != nil {
		t.Fatal(err)
	}

	second, worker := streamExampleModule(t, "worker")
	stream := worker.Globals["jobs"]
	reclaimed := streamTestClaim(t, second, stream, "worker-b", 2)
	streamTestCall(t, second, worker.Globals["process"], reclaimed)
	countResults(1)

	// Bypass producer validation to exercise rejection in the consumer.
	streamTestCall(t, second, stream.NativeObject.Attrs["publish"], StringValue("bad schema"))
	streamTestCall(t, second, worker.Globals["process"], streamTestRead(t, second, stream, "worker-b"))
	send(`{"version":1,"kind":"report","job_id":"def","title":"unavailable"}`, 202)
	streamTestCall(t, second, worker.Globals["process"], streamTestRead(t, second, stream, "worker-b"))
	for attempt := int64(2); attempt <= 3; attempt++ {
		retry := streamTestClaim(t, second, stream, "worker-b", attempt)
		streamTestCall(t, second, worker.Globals["process"], retry)
	}
	countResults(1)
	stats := streamTestCall(t, second, stream.NativeObject.Attrs["stats"])
	for _, entry := range stats.Dict.Entries {
		if entry.Value.Int != 0 {
			t.Fatalf("queue should be idle, %s=%d", entry.Key.Str, entry.Value.Int)
		}
	}
	raw := worker.Globals["cache"].NativeObject.data.(*redisConnection).client
	streamKey := "typhon:stream:{" + streamName + "}"
	dead, err := raw.XRange(ctx, streamKey+":dlq:reports", "-", "+").Result()
	if err != nil || len(dead) != 2 || dead[0].Values["reason"] != "invalid_schema" ||
		dead[1].Values["reason"] != "attempts_exhausted" || dead[1].Values["attempts"] != "3" {
		t.Fatalf("unexpected dead-letter result: %v, %v", dead, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := raw.Del(cleanup, streamKey, streamKey+":dlq:reports").Err(); err != nil {
			t.Error(err)
		}
	})
}
