package typhon

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redistest "github.com/alicebob/miniredis/v2/server"
)

func checkRedisPipeline(t *testing.T, dsn string) {
	t.Helper()
	result, err := captureRun(t, sourceFile(t, fmt.Sprintf(`import typhon.redis as r
db: r.Connection = r.connect_with_pool(%q, 4, 1, 1, 1, 1)
key: str = %q
db.set(key, '0', 60)
before: int = db.stats()['operations']
commands: list[list[str]] = []
i: int = 0
while i < 1000:
    commands.append(['INCR', key])
    i = i + 1
results: list[r.Result] = db.pipeline(commands)
print(db.stats()['operations'] - before)
print(len(results), results[0].value, results[499].value, results[999].value, db.get(key))
before = db.stats()['operations']
i = 0
while i < 1000:
    db.incr(key)
    i = i + 1
print(db.stats()['operations'] - before, db.get(key))
mixed: list[r.Result] = db.pipeline([['HGET', key, 'field'], ['GET', key], ['GET', key + '-missing']])
print(mixed[0].ok, mixed[0].error, mixed[1].value, mixed[2].ok, mixed[2].value)
db.mset({key: 'fresh', key + '-other': 'other'})
print(db.mget([key + '-other', key + '-missing', key]))
print(db.hset(key + '-hash', 'field', 'value'), db.hget(key + '-hash', 'field'))
db.delete(key)
db.delete(key + '-other')
db.delete(key + '-hash')
`, dsn, "typhon:pipeline:"+rand.Text())))
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n1000 1 500 1000 1000\n1000 2000\nFalse redis_error 2000 True None\n[other, None, fresh]\n1 value\n"
	if result != want {
		t.Fatalf("result or operation ordering: %q", result)
	}
}

func TestNativeRedisPipeline(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("typhon", "test-password")
	checkRedisPipeline(t, "redis://typhon:test-password@"+server.Addr()+"/0")
	deadline := time.Now().Add(time.Second)
	for server.CurrentConnectionCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.CurrentConnectionCount() != 0 {
		t.Fatal("Engine shutdown left Redis connections open")
	}
}

func TestNativeRedisPipelineIntegration(t *testing.T) {
	dsn := os.Getenv("TYPHON_TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("set TYPHON_TEST_REDIS_URL to exercise the pipeline on Redis")
	}
	checkRedisPipeline(t, dsn)
}

func TestNativeRedisConfiguredTimeoutNeverReplays(t *testing.T) {
	server := miniredis.RunT(t)
	release := make(chan struct{})
	defer close(release)
	var writes atomic.Int32
	if err := server.Server().Register("TYPHONHANG", func(peer *redistest.Peer, _ string, _ []string) {
		writes.Add(1)
		<-release
		peer.WriteInline("OK")
	}); err != nil {
		t.Fatal(err)
	}
	result, err := captureRun(t, sourceFile(t, fmt.Sprintf(`import typhon.redis as r
db: r.Connection = r.connect_with_pool(%q, 2, 0, 1, 0.1, 1)
try:
    db.command(['TYPHONHANG'])
except TimeoutError:
    print(db.stats()['timeouts'], db.ping())
try:
    db.pipeline([['SET', 'no-partial', 'changed'], ['MULTI']])
except ValidationError:
    print(db.get('no-partial'))
`, "redis://"+server.Addr())))
	if err != nil || strings.TrimSpace(result) != "1 PONG\nNone" || writes.Load() != 1 {
		t.Fatalf("timeout, replay or validation-before-write failure: output=%q writes=%d err=%v", result, writes.Load(), err)
	}
}
