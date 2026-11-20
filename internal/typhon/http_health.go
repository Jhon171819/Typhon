package typhon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	probeInterval = time.Second
	probeTimeout  = 300 * time.Millisecond
	probeMaxAge   = 3 * time.Second
)

type dependencyProbe struct {
	name   string
	ping   func(context.Context) error
	close  func() error
	streak int
	state  string
}

func (p *dependencyProbe) observe(err error) {
	switch {
	case err == nil:
		p.streak = min(2, max(0, p.streak)+1)
		if p.streak == 2 {
			p.state = "healthy"
		}
	default:
		p.streak = max(-2, min(0, p.streak)-1)
		if p.streak == -2 {
			p.state = "unavailable"
		}
	}
}

type healthSnapshot struct {
	at     time.Time
	ready  bool
	states map[string]string
}

type httpHealth struct {
	diagnostics string
	token       [sha256.Size]byte
	probes      []dependencyProbe
	snapshot    atomic.Pointer[healthSnapshot]
	stop        context.CancelFunc
	done        chan struct{}
}

func newHTTPHealth(ctx context.Context, readiness, liveness string) (*httpHealth, error) {
	h := &httpHealth{diagnostics: os.Getenv("TYPHON_HEALTH_DIAGNOSTICS_PATH")}
	if token := os.Getenv("TYPHON_HEALTH_DIAGNOSTICS_TOKEN"); h.diagnostics != "" {
		if !validHealthPath(h.diagnostics) || h.diagnostics == readiness || h.diagnostics == liveness {
			return nil, errors.New("TYPHON_HEALTH_DIAGNOSTICS_PATH must be an absolute HTTP path distinct from other probes")
		}
		if len(token) < 32 || !validIdempotencyKey(token) {
			return nil, errors.New("TYPHON_HEALTH_DIAGNOSTICS_TOKEN requires 32 to 128 portable ASCII characters")
		}
		h.token = sha256.Sum256([]byte(token))
	}

	if dsn := os.Getenv("TYPHON_HEALTH_POSTGRES_URL"); dsn != "" {
		config, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return nil, errors.New("TYPHON_HEALTH_POSTGRES_URL is invalid")
		}
		config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
		config.ConnConfig.ConnectTimeout = probeTimeout
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			return nil, errors.New("could not initialize PostgreSQL health probe")
		}
		h.probes = append(h.probes, dependencyProbe{name: "postgresql", ping: pool.Ping, close: func() error {
			pool.Close()
			return nil
		}})
	}
	if dsn := os.Getenv("TYPHON_HEALTH_REDIS_URL"); dsn != "" {
		config, err := redis.ParseURL(dsn)
		if err != nil || (config.TLSConfig != nil && config.TLSConfig.InsecureSkipVerify) {
			h.close()
			return nil, errors.New("TYPHON_HEALTH_REDIS_URL is invalid or disables TLS verification")
		}
		config.ContextTimeoutEnabled = true
		config.PoolSize, config.MaxActiveConns, config.MaxRetries, config.DialerRetries = 1, 1, -1, 1
		config.DialTimeout, config.ReadTimeout, config.WriteTimeout, config.PoolTimeout = probeTimeout, probeTimeout, probeTimeout, probeTimeout
		client := redis.NewClient(config)
		h.probes = append(h.probes, dependencyProbe{name: "redis", close: client.Close, ping: func(ctx context.Context) error {
			return client.Ping(ctx).Err()
		}})
	}
	if len(h.probes) > 0 {
		h.update(time.Time{})
		ctx, h.stop = context.WithCancel(ctx)
		h.done = make(chan struct{})
		go h.run(ctx)
	}
	return h, nil
}

func (h *httpHealth) update(at time.Time) {
	snapshot := &healthSnapshot{at: at, ready: true, states: make(map[string]string, len(h.probes))}
	for _, probe := range h.probes {
		state := probe.state
		if state == "" {
			state = "checking"
		}
		snapshot.states[probe.name] = state
		snapshot.ready = snapshot.ready && state == "healthy"
	}
	h.snapshot.Store(snapshot)
}

func (h *httpHealth) run(ctx context.Context) {
	defer close(h.done)
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		for i := range h.probes {
			probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
			probe := &h.probes[i]
			probe.observe(probe.ping(probeContext))
			cancel()
		}
		h.update(time.Now())
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (h *httpHealth) close() {
	if h.stop != nil {
		h.stop()
		<-h.done
	}
	for i := len(h.probes) - 1; i >= 0; i-- {
		probe := &h.probes[i]
		reportCleanupError("HTTP health "+probe.name, probe.close())
	}
}

func (h *httpHealth) status() (bool, map[string]string) {
	if h == nil {
		return true, nil
	}
	snapshot := h.snapshot.Load()
	if snapshot == nil {
		return true, nil
	}
	if !snapshot.at.IsZero() && time.Since(snapshot.at) > probeMaxAge {
		stale := make(map[string]string, len(snapshot.states))
		for name := range snapshot.states {
			stale[name] = "stale"
		}
		return false, stale
	}
	return snapshot.ready, snapshot.states
}

func (h *httpHealth) writeDiagnostics(w http.ResponseWriter, r *http.Request, ready, draining, capacity bool, dependencies map[string]string) {
	token := sha256.Sum256([]byte(bearerToken(r.Header.Values("Authorization"))))
	if subtle.ConstantTimeCompare(token[:], h.token[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(struct {
			Ready        bool              `json:"ready"`
			Draining     bool              `json:"draining"`
			Capacity     bool              `json:"queue_has_capacity"`
			Dependencies map[string]string `json:"dependencies,omitempty"`
		}{ready, draining, capacity, dependencies})
	}
}
