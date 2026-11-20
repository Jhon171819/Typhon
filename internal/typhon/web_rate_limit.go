package typhon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const maxRateLimitIdentities = 4096

// Both keys use the same route hash tag. The counter and its index entry
// expire together; denied requests cannot extend the window.
var webRateScript = redis.NewScript(`
local limit, window, capacity = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local current = redis.call('GET', KEYS[2])
if current then
    local ttl = redis.call('PTTL', KEYS[2])
    if ttl > 0 then
        local count = tonumber(current)
        if count >= limit then return {0, 0, ttl} end
        redis.call('INCR', KEYS[2])
        return {1, limit - count - 1, ttl}
    elseif ttl == -1 then
        return redis.error_reply('rate limit counter is missing its TTL')
    end
end
local server_time = redis.call('TIME')
local now = tonumber(server_time[1]) * 1000 + math.floor(tonumber(server_time[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= capacity then
    local soonest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
    return {0, 0, math.max(1, tonumber(soonest[2]) - now)}
end
redis.call('ZADD', KEYS[1], now + window, KEYS[2])
redis.call('PEXPIRE', KEYS[1], window)
redis.call('SET', KEYS[2], 1, 'PX', window)
return {1, limit - 1, window}
`)

type webRateLimiter struct {
	cache     *redisConnection
	namespace string
	limit     int64
	windowMS  int64
	failOpen  bool
	plans     map[string]int64
}

type webRouteRate struct {
	limiter *webRateLimiter
	prefix  string
}

func webRateLimiterType() Type { return Type{Kind: TypeNative, Name: "typhon.web.RateLimiter"} }

func makeWebRateLimiter(vm *VM, args []Value) (Value, error) {
	cache, ok := args[0].NativeObject.data.(*redisConnection)
	if !ok || cache.closed.Load() || args[0].NativeObject.request != nil {
		return NoneValue(), errors.New("rate limiting requires an open Engine-scoped Redis client")
	}
	limit, seconds := args[2].Int, args[3].Int
	if !validPermissionName(args[1].Str) || limit < 1 || limit > 1000000 || seconds < 1 || seconds > 3600 {
		return NoneValue(), errors.New("invalid rate limit namespace, capacity or interval")
	}
	p := &webRateLimiter{cache: cache, namespace: args[1].Str, limit: limit, windowMS: seconds * 1000}
	if len(args) > 4 {
		p.failOpen = args[4].Bool
	}
	if len(args) > 5 {
		if len(args[5].Dict.Entries) > 32 {
			return NoneValue(), errors.New("too many rate limit plans")
		}
		p.plans = map[string]int64{}
		for _, entry := range args[5].Dict.Entries {
			if !validPermissionName(entry.Key.Str) || entry.Value.Int < 1 || entry.Value.Int > 1000000 {
				return NoneValue(), errors.New("invalid rate limit plan scope or capacity")
			}
			p.plans[entry.Key.Str] = entry.Value.Int
		}
	}
	value := nativeObject(vm, webRateLimiterType(), nil)
	value.NativeObject.data = p
	return value, nil
}

func (p *webRateLimiter) bind(method, pattern string) *webRouteRate {
	key := sha256.Sum256([]byte(p.namespace + "\x00" + method + "\x00" + pattern))
	return &webRouteRate{limiter: p, prefix: fmt.Sprintf("typhon:rate:{%x}", key)}
}

func (route *webRouteRate) take(vm *VM, principal verifiedRequestIdentity) (http.Header, bool, error) {
	p := route.limiter
	if p.cache.closed.Load() {
		return nil, false, &Exception{kind: connectionKind, message: "Redis rate limit client is closed"}
	}
	limit, matched := p.limit, false
	for scope, planLimit := range p.plans {
		if principal.scopes[scope] {
			if !matched {
				limit, matched = planLimit, true
			} else {
				limit = min(planLimit, limit)
			}
		}
	}
	fingerprint := sha256.Sum256([]byte(principal.tenant + "\x00" + principal.subject))
	keys := []string{route.prefix + ":active", fmt.Sprintf("%s:caller:%x", route.prefix, fingerprint)}
	ctx, end := p.cache.start(vm.context(), 150*time.Millisecond)
	reply, err := webRateScript.Run(ctx, p.cache.client, keys, limit, p.windowMS, maxRateLimitIdentities).Int64Slice()
	end(err)
	if err != nil {
		return nil, false, driverFailure("Redis", databaseKind, err)
	}
	if len(reply) != 3 || reply[0] < 0 || reply[0] > 1 || reply[1] < 0 || reply[1] >= limit ||
		reply[2] < 1 || reply[2] > p.windowMS {
		return nil, false, errors.New("invalid Redis rate limit response")
	}
	allowed := reply[0] == 1
	wait := (reply[2] + 999) / 1000
	headers := http.Header{}
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-RateLimit-Limit", strconv.FormatInt(limit, 10))
	headers.Set("X-RateLimit-Remaining", strconv.FormatInt(reply[1], 10))
	headers.Set("X-RateLimit-Reset", strconv.FormatInt(wait, 10))
	if !allowed {
		headers.Set("Retry-After", strconv.FormatInt(wait, 10))
	}
	return headers, allowed, nil
}
