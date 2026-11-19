package typhon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisDriverTypes() map[string]Type {
	connection := Type{Kind: TypeNative, Name: "typhon.redis.Connection", Members: map[string]Type{
		"ping":    nativeFuncType("redis.ping", nil, StrType(), 0),
		"get":     nativeFuncType("redis.get", []Type{StrType()}, unionType(StrType(), NoneType()), 0),
		"set":     nativeFuncType("redis.set", []Type{StrType(), StrType(), FloatType()}, NoneType(), 1),
		"delete":  nativeFuncType("redis.delete", []Type{StrType()}, IntType(), 0),
		"exists":  nativeFuncType("redis.exists", []Type{StrType()}, BoolType(), 0),
		"incr":    nativeFuncType("redis.incr", []Type{StrType()}, IntType(), 0),
		"ttl":     nativeFuncType("redis.ttl", []Type{StrType()}, FloatType(), 0),
		"command": nativeFuncType("redis.command", []Type{listType(StrType())}, AnyType(), 0),
		"close":   nativeFuncType("redis.close", nil, NoneType(), 0),
	}}
	nullableString := unionType(StrType(), NoneType())
	result := Type{Kind: TypeNative, Name: "typhon.redis.Result", Members: map[string]Type{
		"ok": BoolType(), "value": AnyType(), "error": nullableString,
	}}
	connection.Members["pipeline"] = nativeFuncType("redis.pipeline", []Type{listType(listType(StrType()))}, listType(result), 0)
	connection.Members["mget"] = nativeFuncType("redis.mget", []Type{listType(StrType())}, listType(nullableString), 0)
	connection.Members["mset"] = nativeFuncType("redis.mset", []Type{dictType(StrType(), StrType())}, NoneType(), 0)
	connection.Members["hget"] = nativeFuncType("redis.hget", []Type{StrType(), StrType()}, nullableString, 0)
	connection.Members["hset"] = nativeFuncType("redis.hset", []Type{StrType(), StrType(), StrType()}, IntType(), 0)
	connection.Members["stats"] = nativeFuncType("redis.stats", nil, dictType(StrType(), IntType()), 0)
	return map[string]Type{
		"Connection":        connection,
		"Result":            result,
		"connect":           nativeFuncType("redis.connect", []Type{StrType(), FloatType()}, connection, 1),
		"connect_with_pool": nativeFuncType("redis.connect_with_pool", []Type{StrType(), IntType(), IntType(), FloatType(), FloatType(), FloatType()}, connection, 3),
		"env":               nativeFuncType("redis.env", []Type{StrType()}, StrType(), 0),
	}
}

func redisDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["connect"] = driverFunc(types["connect"], nativeRedisConnect)
	exports["connect_with_pool"] = driverFunc(types["connect_with_pool"], func(vm *VM, args []Value) (Value, error) {
		if args[1].Int < 1 || args[1].Int > 64 || args[2].Int < 0 || args[2].Int > args[1].Int {
			return NoneValue(), errors.New("Redis pool requires 1 to 64 connections and min_idle no greater than the pool")
		}
		dial, err := driverTimeout(args, 3)
		if err != nil {
			return NoneValue(), err
		}
		read, err := driverTimeout(args, 4)
		if err != nil {
			return NoneValue(), err
		}
		write, err := driverTimeout(args, 5)
		if err != nil {
			return NoneValue(), err
		}
		return connectRedis(vm, args[0].Str, int(args[1].Int), int(args[2].Int), dial, read, write, true)
	})
	exports["env"] = driverFunc(types["env"], driverEnv)
	return exports
}

type redisConnection struct {
	client    *redis.Client
	operation atomic.Pointer[context.Context]
	closed    atomic.Bool
	timeout   time.Duration
	// Only the owner Engine executes commands or reads these counters.
	commands, latencyNS, timeouts, networkErrors int64
}

func (db *redisConnection) start(ctx context.Context, timeout time.Duration) (context.Context, func(error)) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, min(timeout, db.timeout))
	db.operation.Store(&ctx)
	return ctx, func(err error) {
		db.operation.Store(nil)
		cancel()
		db.commands++
		db.latencyNS += time.Since(start).Nanoseconds()
		if err == nil {
			return
		}
		var ex *Exception
		if !errors.As(err, &ex) {
			errors.As(driverFailure("Redis", databaseKind, err), &ex)
		}
		if ex != nil {
			switch ex.kind {
			case timeoutKind:
				db.timeouts++
			case connectionKind:
				db.networkErrors++
			}
		}
	}
}

func (db *redisConnection) stats() Value {
	pool := db.client.PoolStats()
	stats := DictValue()
	for _, item := range []struct {
		name string
		val  int64
	}{
		{"operations", db.commands}, {"latency_ns", db.latencyNS}, {"timeouts", db.timeouts}, {"network_errors", db.networkErrors},
		{"total_connections", int64(pool.TotalConns)}, {"idle_connections", int64(pool.IdleConns)},
		{"in_use_connections", max(0, int64(pool.TotalConns)-int64(pool.IdleConns))},
	} {
		_ = stats.Dict.put(StringValue(item.name), IntValue(item.val))
	}
	return stats
}

func nativeRedisConnect(vm *VM, args []Value) (Value, error) {
	timeout, err := driverTimeout(args, 1)
	if err != nil {
		return NoneValue(), err
	}
	return connectRedis(vm, args[0].Str, 16, 0, timeout, timeout, timeout, false)
}

func connectRedis(vm *VM, dsn string, poolSize, minIdle int, dialTimeout, read, write time.Duration, configured bool) (Value, error) {
	options, err := redis.ParseURL(dsn)
	if err != nil {
		return NoneValue(), driverFailure("Redis", validationKind, err)
	}
	if configured && options.TLSConfig != nil && options.TLSConfig.InsecureSkipVerify {
		return NoneValue(), errors.New("configured Redis TLS requires certificate verification")
	}
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = dialTimeout, read, write
	options.ContextTimeoutEnabled = true
	options.PoolSize = poolSize
	if configured {
		options.MaxActiveConns, options.MinIdleConns = poolSize, minIdle
		options.DialerRetries = 1
	}
	options.MaxRetries = -1 // A failed write is not retried automatically.
	options.Protocol = 2    // RESP2 arrays fit Typhon lists; maps are not yet supported.
	db := &redisConnection{timeout: max(dialTimeout, read, write)}
	dial := redis.NewDialer(options)
	if options.TLSConfig != nil {
		tlsDial := &tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout}, Config: options.TLSConfig}
		dial = tlsDial.DialContext
	}
	options.Dialer = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &redisContextConn{Conn: conn, operation: &db.operation}, nil
	}
	client := redis.NewClient(options)
	db.client = client
	ctx, done := db.start(vm.context(), db.timeout)
	err = client.Ping(ctx).Err()
	done(err)
	if err != nil {
		reportCleanupError("Redis client after failed ping", driverFailure("Redis", databaseKind, client.Close()))
		return NoneValue(), driverFailure("Redis", databaseKind, err)
	}
	scope := vm.resourceScope()
	close := func(context.Context) error {
		if db.closed.Swap(true) {
			return nil
		}
		scope.remove(db)
		return driverFailure("Redis", databaseKind, client.Close())
	}
	if err := scope.add(vm.context(), db, "Redis client close", close); err != nil {
		return NoneValue(), err
	}
	typ := redisDriverTypes()["Connection"]
	attrs := map[string]Value{"close": driverCloser(typ.Members["close"], close)}
	attrs["stats"] = driverFunc(typ.Members["stats"], func(vm *VM, args []Value) (Value, error) {
		return db.stats(), nil
	})
	for _, name := range []string{"ping", "get", "set", "delete", "exists", "incr", "ttl", "command", "pipeline", "mget", "mset", "hget", "hset"} {
		attrs[name] = driverFunc(typ.Members[name], func(vm *VM, args []Value) (_ Value, runErr error) {
			if db.closed.Load() {
				return NoneValue(), fmt.Errorf("Redis connection is closed")
			}
			ctx, done := db.start(vm.context(), db.timeout)
			var transportErr error
			defer func() { done(errors.Join(transportErr, runErr)) }()
			switch name {
			case "ping":
				value, err := client.Ping(ctx).Result()
				return StringValue(value), driverFailure("Redis", databaseKind, err)
			case "get", "hget":
				var value string
				var err error
				if name == "get" {
					value, err = client.Get(ctx, args[0].Str).Result()
				} else {
					value, err = client.HGet(ctx, args[0].Str, args[1].Str).Result()
				}
				if err == redis.Nil {
					return NoneValue(), nil
				}
				if err == nil {
					err = vm.chargeBytes(int64(len(value)))
				}
				return StringValue(value), driverFailure("Redis", databaseKind, err)
			case "set":
				var ttl time.Duration
				if len(args) == 3 {
					seconds := args[2].Float
					if seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) || math.IsInf(seconds, 0) || math.IsNaN(seconds) {
						return NoneValue(), fmt.Errorf("Redis TTL must be finite and non-negative")
					}
					ttl = time.Duration(seconds * float64(time.Second))
					if seconds > 0 && ttl < time.Millisecond {
						return NoneValue(), fmt.Errorf("Redis TTL must be at least 0.001 seconds")
					}
				}
				return NoneValue(), driverFailure("Redis", databaseKind, client.Set(ctx, args[0].Str, args[1].Str, ttl).Err())
			case "delete":
				value, err := client.Del(ctx, args[0].Str).Result()
				return IntValue(value), driverFailure("Redis", databaseKind, err)
			case "exists":
				value, err := client.Exists(ctx, args[0].Str).Result()
				return BoolValue(value > 0), driverFailure("Redis", databaseKind, err)
			case "incr":
				value, err := client.Incr(ctx, args[0].Str).Result()
				return IntValue(value), driverFailure("Redis", databaseKind, err)
			case "hset":
				value, err := client.HSet(ctx, args[0].Str, args[1].Str, args[2].Str).Result()
				return IntValue(value), driverFailure("Redis", databaseKind, err)
			case "mget":
				keys := make([]string, len(*args[0].List))
				for i, key := range *args[0].List {
					keys[i] = key.Str
				}
				if len(keys) == 0 {
					return ListValue(nil), nil
				}
				result, err := client.MGet(ctx, keys...).Result()
				if err != nil {
					return NoneValue(), driverFailure("Redis", databaseKind, err)
				}
				return redisValue(vm, result, 0)
			case "mset":
				pairs := make([]any, 0, 2*len(args[0].Dict.Entries))
				for _, pair := range args[0].Dict.Entries {
					pairs = append(pairs, pair.Key.Str, pair.Value.Str)
				}
				if len(pairs) == 0 {
					return NoneValue(), nil
				}
				return NoneValue(), driverFailure("Redis", databaseKind, client.MSet(ctx, pairs...).Err())
			case "pipeline":
				v, transport, err := redisPipeline(vm, ctx, client, *args[0].List)
				transportErr = transport
				return v, err
			case "ttl":
				value, err := client.PTTL(ctx, args[0].Str).Result()
				// go-redis preserves -1/-2 as nanosecond sentinels.
				if value < 0 {
					return FloatValue(float64(value)), driverFailure("Redis", databaseKind, err)
				}
				return FloatValue(value.Seconds()), driverFailure("Redis", databaseKind, err)
			case "command":
				items := *args[0].List
				if len(items) == 0 {
					return NoneValue(), fmt.Errorf("Redis command must not be empty")
				}
				switch strings.ToUpper(items[0].Str) {
				case "MULTI", "EXEC", "DISCARD", "WATCH", "UNWATCH", "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "UNSUBSCRIBE", "PUNSUBSCRIBE", "SUNSUBSCRIBE", "MONITOR", "SELECT", "AUTH", "HELLO", "CLIENT", "QUIT", "RESET":
					return NoneValue(), fmt.Errorf("Redis command changes connection state and cannot use the shared pool")
				}
				command := make([]any, len(items))
				for i, item := range items {
					command[i] = item.Str
				}
				value, err := client.Do(ctx, command...).Result()
				if err == redis.Nil {
					return NoneValue(), nil
				}
				if err != nil {
					return NoneValue(), driverFailure("Redis", databaseKind, err)
				}
				return redisValue(vm, value, 0)
			}
			return NoneValue(), fmt.Errorf("unknown Redis operation")
		})
	}
	value := scopedNativeObject(vm, typ, attrs)
	value.NativeObject.data = db
	return value, nil
}

func redisValue(vm *VM, value any, depth int) (Value, error) {
	if err := vm.executionError(); err != nil {
		return NoneValue(), err
	}
	if depth > 64 {
		return NoneValue(), fmt.Errorf("Redis response depth limit exceeded")
	}
	if err := vm.chargeBytes(valueSlotBytes); err != nil {
		return NoneValue(), err
	}
	switch value := value.(type) {
	case nil:
		return NoneValue(), nil
	case bool:
		return BoolValue(value), nil
	case int64:
		return IntValue(value), nil
	case string:
		if err := vm.chargeBytes(int64(len(value))); err != nil {
			return NoneValue(), err
		}
		return StringValue(value), nil
	case []byte:
		return StringValue(string(value)), nil
	case []any:
		items := make([]Value, len(value))
		for i, item := range value {
			converted, err := redisValue(vm, item, depth+1)
			if err != nil {
				return NoneValue(), err
			}
			items[i] = converted
		}
		return ListValue(items), nil
	default:
		return NoneValue(), fmt.Errorf("unsupported Redis response type %T", value)
	}
}
