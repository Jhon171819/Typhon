package typhon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

const maxRedisCacheBytes = 128 << 10

const redisCacheLookup = `local value = redis.call('GET', KEYS[1])
if not value then
  redis.call('PSETEX', KEYS[1], ARGV[2], ARGV[1])
  return ARGV[1]
end
if string.len(value) > tonumber(ARGV[3]) then
  return redis.error_reply('ERR cache payload exceeds the limit')
end
return value`

const redisCacheStore = `if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('PSETEX', KEYS[1], ARGV[3], ARGV[2])
return 1`

type redisCache struct {
	db      *redisConnection
	domain  string
	version int64
	ttl     time.Duration
}

func redisCacheTypes() (Type, Type) {
	request := Type{Kind: TypeNative, Name: "typhon.http.Request"}
	entry := Type{Kind: TypeNative, Name: "typhon.redis.CacheEntry", Members: map[string]Type{
		"hit": BoolType(), "value": AnyType(),
		"store": nativeFuncType("redis.cache.store", []Type{request, AnyType()}, BoolType(), 0),
	}}
	cache := Type{Kind: TypeNative, Name: "typhon.redis.Cache", Members: map[string]Type{
		"lookup":     nativeFuncType("redis.cache.lookup", []Type{request, StrType()}, entry, 0),
		"invalidate": nativeFuncType("redis.cache.invalidate", []Type{request, StrType()}, NoneType(), 0),
	}}
	return cache, entry
}

func (db *redisConnection) newCache(vm *VM, args []Value) (Value, error) {
	cache := redisCache{db: db, domain: args[0].Str, version: args[1].Int, ttl: time.Minute}
	if !validAuthText(cache.domain, 64) || cache.version < 1 || cache.version > 2147483647 {
		return NoneValue(), errors.New("Redis cache requires a domain of 1 to 64 bytes and a positive 31-bit schema version")
	}
	if len(args) == 3 {
		seconds := args[2].Float
		if !(seconds >= 0.001 && seconds <= 86400) {
			return NoneValue(), errors.New("Redis cache TTL must be between 0.001 and 86400 seconds")
		}
		cache.ttl = time.Duration(seconds*1000) * time.Millisecond
	}
	typ, _ := redisCacheTypes()
	return nativeObject(vm, typ, map[string]Value{
		"lookup":     driverFunc(typ.Members["lookup"], cache.lookup),
		"invalidate": driverFunc(typ.Members["invalidate"], cache.invalidate),
	}), nil
}

func (c redisCache) key(state *webRequest, logical string) (string, error) {
	if !validAuthText(logical, 256) {
		return "", errors.New("Redis cache logical key must have 1 to 256 UTF-8 bytes without control characters")
	}
	encode := base64.RawURLEncoding.EncodeToString
	return "typhon:cache:tenant:" + encode([]byte(state.principal.tenant)) + ":" +
		encode([]byte(c.domain)) + ":" + strconv.FormatInt(c.version, 10) + ":" + encode([]byte(logical)), nil
}

func redisCacheRequest(vm *VM, request Value) (*webRequest, error) {
	state, err := authorizedTenantRequest(vm, request)
	if err != nil {
		return nil, err
	}
	if expires := state.principal.expires; !expires.IsZero() && !time.Now().Before(expires) {
		return nil, (&webRejection{status: 403}).exception()
	}
	return state, nil
}

func (c redisCache) run(vm *VM, operation func(context.Context) error) error {
	err := c.db.run(vm.context(), c.db.timeout, operation)
	if err != nil {
		c.db.cacheErrors++
	}
	return err
}

func (c redisCache) lookup(vm *VM, args []Value) (Value, error) {
	state, err := redisCacheRequest(vm, args[0])
	if err != nil {
		return NoneValue(), err
	}
	key, err := c.key(state, args[1].Str)
	if err != nil {
		return NoneValue(), err
	}
	var raw string
	if err := c.run(vm, func(ctx context.Context) error {
		var err error
		raw, err = c.db.client.Eval(ctx, redisCacheLookup, []string{key},
			"!"+rand.Text(), min(c.ttl, 10*time.Second).Milliseconds(), maxRedisCacheBytes).Text()
		return err
	}); err != nil {
		return NoneValue(), err
	}
	// JSON can never begin with '!'. A miss carries a short-lived fencing token.
	hit := !strings.HasPrefix(raw, "!")
	value := NoneValue()
	lease := ""
	if hit {
		value, err = loadJSON(vm, raw)
		if err != nil {
			c.db.cacheErrors++
			return NoneValue(), err
		}
		c.db.cacheHits++
	} else {
		lease = raw
		c.db.cacheMisses++
	}
	_, typ := redisCacheTypes()
	return scopedNativeObject(vm, typ, map[string]Value{
		"hit":   BoolValue(hit),
		"value": value,
		"store": driverFunc(typ.Members["store"], func(vm *VM, args []Value) (Value, error) {
			if _, err := redisCacheRequest(vm, args[0]); err != nil {
				return NoneValue(), err
			}
			return c.store(vm, key, lease, args[1])
		}),
	}), nil
}

func (c redisCache) store(vm *VM, key, lease string, value Value) (Value, error) {
	if lease == "" {
		return BoolValue(false), nil
	}
	raw, err := dumpJSON(vm, value)
	if err != nil {
		return NoneValue(), err
	}
	if len(raw) > maxRedisCacheBytes {
		return NoneValue(), errors.New("Redis cache payload exceeds 128 KiB")
	}
	var stored int64
	err = c.run(vm, func(ctx context.Context) error {
		var err error
		stored, err = c.db.client.Eval(ctx, redisCacheStore, []string{key}, lease, raw, c.ttl.Milliseconds()).Int64()
		return err
	})
	return BoolValue(stored == 1), err
}

func (c redisCache) invalidate(vm *VM, args []Value) (Value, error) {
	state, err := redisCacheRequest(vm, args[0])
	if err != nil {
		return NoneValue(), err
	}
	key, err := c.key(state, args[1].Str)
	if err != nil {
		return NoneValue(), err
	}
	if err := c.run(vm, func(ctx context.Context) error {
		return c.db.client.Del(ctx, key).Err()
	}); err != nil {
		return NoneValue(), err
	}
	c.db.cacheInvalidations++
	return NoneValue(), nil
}
