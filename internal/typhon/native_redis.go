package typhon

import (
	"fmt"
	"math"
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
	return map[string]Type{
		"Connection": connection,
		"connect":    nativeFuncType("redis.connect", []Type{StrType(), FloatType()}, connection, 1),
		"env":        nativeFuncType("redis.env", []Type{StrType()}, StrType(), 0),
	}
}

func redisDriverExports(types map[string]Type) map[string]Value {
	exports := driverTypeSymbols(types)
	exports["connect"] = driverFunc(types["connect"], nativeRedisConnect)
	exports["env"] = driverFunc(types["env"], driverEnv)
	return exports
}

func nativeRedisConnect(vm *VM, args []Value) (Value, error) {
	timeout, err := driverTimeout(args, 1)
	if err != nil {
		return NoneValue(), err
	}
	options, err := redis.ParseURL(args[0].Str)
	if err != nil {
		return NoneValue(), fmt.Errorf("invalid Redis connection URL")
	}
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = timeout, timeout, timeout
	options.ContextTimeoutEnabled = true
	options.PoolSize = 16
	options.MaxRetries = -1 // A failed write is not retried automatically.
	options.Protocol = 2    // RESP2 arrays fit Typhon lists; maps are not yet supported.
	client := redis.NewClient(options)
	ctx, cancel := driverContext(vm, timeout)
	err = client.Ping(ctx).Err()
	cancel()
	if err != nil {
		_ = client.Close()
		return NoneValue(), err
	}
	var closed atomic.Bool
	scope := vm.resourceScope()
	close := func() error {
		if closed.Swap(true) {
			return nil
		}
		scope.remove(&closed)
		return client.Close()
	}
	if err := scope.add(&closed, close); err != nil {
		return NoneValue(), err
	}
	typ := redisDriverTypes()["Connection"]
	attrs := map[string]Value{"close": driverFunc(typ.Members["close"], func(vm *VM, args []Value) (Value, error) { return NoneValue(), close() })}
	for _, name := range []string{"ping", "get", "set", "delete", "exists", "incr", "ttl", "command"} {
		attrs[name] = driverFunc(typ.Members[name], func(vm *VM, args []Value) (Value, error) {
			if closed.Load() {
				return NoneValue(), fmt.Errorf("Redis connection is closed")
			}
			ctx, cancel := driverContext(vm, timeout)
			defer cancel()
			switch name {
			case "ping":
				value, err := client.Ping(ctx).Result()
				return StringValue(value), err
			case "get":
				value, err := client.Get(ctx, args[0].Str).Result()
				if err == redis.Nil {
					return NoneValue(), nil
				}
				return StringValue(value), err
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
				return NoneValue(), client.Set(ctx, args[0].Str, args[1].Str, ttl).Err()
			case "delete":
				value, err := client.Del(ctx, args[0].Str).Result()
				return IntValue(value), err
			case "exists":
				value, err := client.Exists(ctx, args[0].Str).Result()
				return BoolValue(value > 0), err
			case "incr":
				value, err := client.Incr(ctx, args[0].Str).Result()
				return IntValue(value), err
			case "ttl":
				value, err := client.PTTL(ctx, args[0].Str).Result()
				// go-redis preserves -1/-2 as nanosecond sentinels.
				if value < 0 {
					return FloatValue(float64(value)), err
				}
				return FloatValue(value.Seconds()), err
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
					return NoneValue(), err
				}
				return redisValue(value, 0)
			}
			return NoneValue(), fmt.Errorf("unknown Redis operation")
		})
	}
	return nativeObject(vm, typ, attrs), nil
}

func redisValue(value any, depth int) (Value, error) {
	if depth > 64 {
		return NoneValue(), fmt.Errorf("Redis response depth limit exceeded")
	}
	switch value := value.(type) {
	case nil:
		return NoneValue(), nil
	case bool:
		return BoolValue(value), nil
	case int64:
		return IntValue(value), nil
	case string:
		return StringValue(value), nil
	case []byte:
		return StringValue(string(value)), nil
	case []any:
		items := make([]Value, len(value))
		for i, item := range value {
			converted, err := redisValue(item, depth+1)
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
