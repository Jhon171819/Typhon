package typhon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativeHTTPDrainReadinessAndAdmission(t *testing.T) {
	pool := newHTTPWorkerPool(t.Context(), 1, 1)
	pool.liveness = "/live"
	running := make(chan struct{})
	finish := make(chan struct{})
	gate := newHTTPDrain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(running)
		<-finish
		w.WriteHeader(http.StatusCreated)
	}), pool, "/ready")
	request := func(method, path string) int {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code
	}
	if got := request("GET", "/ready"); got != 200 {
		t.Fatalf("server should be ready before drain, got %d", got)
	}
	completed := make(chan int, 1)
	go func() { completed <- request("POST", "/work") }()
	<-running
	active := gate.start()
	ready, live, newWork := request("GET", "/ready"), request("HEAD", "/live"), request("GET", "/work")
	close(finish)
	status := <-completed
	<-gate.done
	if active != 1 || ready != 503 || live != 204 || newWork != 503 || status != 201 {
		t.Fatalf("drain: active=%d ready=%d live=%d new work=%d active response=%d", active, ready, live, newWork, status)
	}
}
