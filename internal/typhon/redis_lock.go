package typhon

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/redis/go-redis/v9"
)

const redisLockRelease = `if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

type redisLock struct {
	db       *redisConnection
	scope    *resourceScope
	key      string
	token    string
	released bool
}

func redisLockType() Type {
	return Type{Kind: TypeNative, Name: "typhon.redis.Lock", Members: map[string]Type{
		"release": nativeFuncType("redis.lock.release", nil, BoolType(), 0),
	}}
}

func (db *redisConnection) acquireLock(vm *VM, args []Value) (Value, error) {
	key, seconds := args[0].Str, args[1].Float
	if !validAuthText(key, 256) || !(seconds >= 0.001 && seconds <= 60) {
		return NoneValue(), errors.New("Redis lock requires a key of 1 to 256 bytes and TTL between 0.001 and 60 seconds")
	}
	lock := &redisLock{
		db: db, scope: vm.resourceScope(), key: "typhon:lock:" + key, token: rand.Text(),
	}
	var acquired bool
	err := db.run(vm.context(), db.timeout, func(ctx context.Context) error {
		reply, err := db.client.Do(ctx, "SET", lock.key, lock.token, "NX", "PX", int64(seconds*1000)).Text()
		if err == redis.Nil {
			return nil
		}
		acquired = reply == "OK"
		return err
	})
	if err != nil || !acquired {
		return NoneValue(), err
	}
	if err := lock.scope.add(vm.context(), lock, "Redis lock release", func(ctx context.Context) error {
		_, err := lock.release(ctx)
		return err
	}); err != nil {
		return NoneValue(), err
	}
	if err := vm.chargeBytes(valueSlotBytes); err != nil {
		return NoneValue(), err
	}
	typ := redisLockType()
	release := driverFunc(typ.Members["release"], func(vm *VM, _ []Value) (Value, error) {
		released, err := lock.release(vm.context())
		return BoolValue(released), err
	})
	release.Native.cleanup = true
	return scopedNativeObject(vm, typ, map[string]Value{"release": release}), nil
}

func (l *redisLock) release(ctx context.Context) (bool, error) {
	if l.released {
		return false, nil
	}
	var deleted int64
	err := l.db.run(ctx, cleanupTimeout, func(ctx context.Context) error {
		var err error
		deleted, err = l.db.client.Eval(ctx, redisLockRelease, []string{l.key}, l.token).Int64()
		return err
	})
	if err != nil {
		return false, err
	}
	l.released = true
	l.scope.remove(l)
	return deleted == 1, nil
}
