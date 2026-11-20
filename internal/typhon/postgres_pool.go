package typhon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresPoolSettings struct {
	max, min                int32
	idle, lifetime, health  time.Duration
	acquire, query, connect time.Duration
	grace                   time.Duration
	typed, requireTLS       bool
	lazy                    bool
	share                   string
}

type postgresPoolRegistry struct {
	mu    sync.Mutex
	pools map[string]*sharedPostgresPool
}

type sharedPostgresPool struct {
	pool        *pgxpool.Pool
	config      postgresPoolSettings
	fingerprint [32]byte
	refs        int
}

func (r *postgresPoolRegistry) checkout(ctx context.Context, dsn string, config *pgxpool.Config, options postgresPoolSettings) (*pgxpool.Pool, func(), error) {
	if options.share == "" {
		pool, err := openPostgresPool(ctx, config, !options.lazy)
		if err != nil {
			return nil, nil, err
		}
		return pool, pool.Close, nil
	}
	fingerprint := sha256.Sum256([]byte(dsn))
	r.mu.Lock()
	entry := r.pools[options.share]
	if entry != nil && (entry.fingerprint != fingerprint || entry.config != options) {
		r.mu.Unlock()
		return nil, nil, errors.New("shared PostgreSQL pool name already uses a different configuration")
	}
	if entry == nil {
		pool, err := openPostgresPool(ctx, config, !options.lazy)
		if err != nil {
			r.mu.Unlock()
			return nil, nil, err
		}
		entry = &sharedPostgresPool{pool: pool, config: options, fingerprint: fingerprint}
		if r.pools == nil {
			r.pools = make(map[string]*sharedPostgresPool)
		}
		r.pools[options.share] = entry
	}
	entry.refs++
	r.mu.Unlock()
	return entry.pool, func() {
		r.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			entry.pool.Close()
			delete(r.pools, options.share)
		}
		r.mu.Unlock()
	}, nil
}

func openPostgresPool(ctx context.Context, config *pgxpool.Config, probe bool) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, driverFailure("PostgreSQL", connectionKind, err)
	}
	if probe {
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, driverFailure("PostgreSQL", connectionKind, err)
		}
	}
	return pool, nil
}

func parsePostgresPoolSettings(input Value) (postgresPoolSettings, error) {
	p := postgresPoolSettings{
		max: 16, idle: 30 * time.Minute, lifetime: time.Hour, health: time.Minute,
		acquire: 10 * time.Second, query: 10 * time.Second, connect: 10 * time.Second, grace: 500 * time.Millisecond,
	}
	for _, option := range input.Dict.Entries {
		name, value := option.Key.Str, option.Value
		switch name {
		case "max_connections", "min_connections":
			if value.Kind != ValueInt || value.Int < 0 || value.Int > 64 {
				return p, errors.New("PostgreSQL pool sizes must be integers from 0 to 64")
			}
			if name == "max_connections" {
				p.max = int32(value.Int)
			} else {
				p.min = int32(value.Int)
			}
		case "require_tls", "typed", "lazy":
			if value.Kind != ValueBool {
				return p, errors.New("PostgreSQL pool flags must be booleans")
			}
			switch name {
			case "typed":
				p.typed = value.Bool
			case "require_tls":
				p.requireTLS = value.Bool
			case "lazy":
				p.lazy = value.Bool
			}
		case "share":
			if value.Kind != ValueString || !validPermissionName(value.Str) {
				return p, errors.New("invalid PostgreSQL shared pool name")
			}
			p.share = value.Str
		default:
			target, maximum := (*time.Duration)(nil), float64(86400)
			switch name {
			case "idle_seconds":
				target = &p.idle
			case "lifetime_seconds":
				target = &p.lifetime
			case "health_seconds":
				target = &p.health
			case "acquire_seconds":
				target, maximum = &p.acquire, 300
			case "query_seconds":
				target, maximum = &p.query, 300
			case "connect_seconds":
				target, maximum = &p.connect, 300
			case "grace_seconds":
				target, maximum = &p.grace, 2
			default:
				return p, errors.New("unknown PostgreSQL pool option")
			}
			if value.Kind != ValueInt && value.Kind != ValueFloat {
				return p, errors.New("PostgreSQL pool durations must use seconds")
			}
			seconds := numberValue(value)
			if math.IsNaN(seconds) || seconds > maximum ||
				(seconds < 0.001 && !(name == "grace_seconds" && seconds == 0)) {
				return p, errors.New("PostgreSQL pool duration is out of bounds")
			}
			*target = time.Duration(seconds * float64(time.Second))
		}
	}
	if p.max < 1 || p.min > p.max {
		return p, errors.New("PostgreSQL pool must have 1 to 64 max connections and min no greater than max")
	}
	return p, nil
}

func nativePostgresPooled(vm *VM, args []Value) (Value, error) {
	if vm.request != nil {
		return NoneValue(), errors.New("configure PostgreSQL pools outside an HTTP request")
	}
	options, err := parsePostgresPoolSettings(args[1])
	if err != nil {
		return NoneValue(), err
	}
	config, err := postgresPoolConfig(vm, args[0].Str, options.connect)
	if err != nil {
		return NoneValue(), err
	}
	config.MaxConns, config.MinConns, config.MinIdleConns = options.max, options.min, 0
	config.MaxConnIdleTime, config.MaxConnLifetime, config.HealthCheckPeriod = options.idle, options.lifetime, options.health
	config.MaxConnLifetimeJitter = 0
	if options.requireTLS {
		secure := func(config *tls.Config) bool {
			return config != nil && !config.InsecureSkipVerify && config.ServerName != ""
		}
		if !secure(config.ConnConfig.TLSConfig) {
			return NoneValue(), errors.New("PostgreSQL pool requires sslmode=verify-full on every target")
		}
		for _, fallback := range config.ConnConfig.Fallbacks {
			if !secure(fallback.TLSConfig) {
				return NoneValue(), errors.New("PostgreSQL pool requires sslmode=verify-full on every target")
			}
		}
	}
	ctx, cancel := driverContext(vm, options.connect)
	defer cancel()
	pool, release, err := vm.engine.postgresPools.checkout(ctx, args[0].Str, config, options)
	if err != nil {
		return NoneValue(), err
	}
	db := &postgresConnection{
		scope: vm.resourceScope(), pool: pool, timeout: options.query, acquire: options.acquire,
		typed: options.typed, grace: options.grace, release: release,
	}
	return postgresConnectionValue(vm, db)
}

func (db *postgresConnection) stats() Value {
	stats := db.pool.Stat()
	result := DictValue()
	for _, metric := range []struct {
		name  string
		value int64
	}{
		{"opened", int64(stats.TotalConns())}, {"idle", int64(stats.IdleConns())}, {"in_use", int64(stats.AcquiredConns())},
		{"max_connections", int64(stats.MaxConns())}, {"waited_acquisitions", stats.EmptyAcquireCount()},
		{"wait_ns", stats.EmptyAcquireWaitTime().Nanoseconds()}, {"cancelled_acquisitions", stats.CanceledAcquireCount()},
	} {
		_ = result.Dict.put(StringValue(metric.name), IntValue(metric.value))
	}
	return result
}
