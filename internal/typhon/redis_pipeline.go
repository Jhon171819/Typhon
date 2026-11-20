package typhon

import (
	"context"
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

const maxRedisPipelineCommands = 1000
const maxRedisPipelineBytes = 2 << 20

func redisPipeline(vm *VM, ctx context.Context, client *redis.Client, commands []Value) (Value, error, error) {
	if len(commands) > maxRedisPipelineCommands {
		return NoneValue(), nil, errors.New("Redis pipeline exceeds 1000 commands")
	}
	batch := client.Pipeline()
	queued := make([]*redis.Cmd, len(commands))
	var bytes int
	for i, command := range commands {
		args := *command.List
		if len(args) == 0 || len(args) > 1024 {
			return NoneValue(), nil, errors.New("Redis pipeline requires 1 to 1024 strings per command")
		}
		switch strings.ToUpper(args[0].Str) {
		case "PING", "GET", "SET", "DEL", "EXISTS", "INCR", "DECR", "EXPIRE", "TTL", "PTTL",
			"MGET", "MSET", "HGET", "HSET", "HEXISTS", "HDEL":
		default:
			return NoneValue(), nil, errors.New("command is unavailable in a Redis pipeline")
		}
		values := make([]any, len(args))
		for j, arg := range args {
			bytes += len(arg.Str) + 16
			if bytes > maxRedisPipelineBytes {
				return NoneValue(), nil, errors.New("Redis pipeline exceeds 2 MiB of command data")
			}
			values[j] = arg.Str
		}
		queued[i] = batch.Do(ctx, values...)
	}
	_, batchErr := batch.Exec(ctx)
	if err := vm.executionError(); err != nil {
		return NoneValue(), batchErr, err
	}
	var transport error
	typ := redisDriverTypes()["Result"]
	results := make([]Value, len(queued))
	for i, result := range queued {
		value, err := result.Result()
		data, message := NoneValue(), NoneValue()
		ok := err == nil || errors.Is(err, redis.Nil)
		if err == nil {
			var conversionErr error
			data, conversionErr = redisValue(vm, value, 0)
			if conversionErr != nil {
				return NoneValue(), transport, conversionErr
			}
		} else if !ok {
			var server redis.Error
			if errors.As(err, &server) {
				message = StringValue("redis_error")
			} else {
				transport = err
				message = StringValue("outcome_unknown")
			}
		}
		if err := vm.chargeBytes(4 * valueSlotBytes); err != nil {
			return NoneValue(), transport, err
		}
		results[i] = nativeObject(vm, typ, map[string]Value{
			"ok": BoolValue(ok), "value": data, "error": message,
		})
	}
	return ListValue(results), transport, nil
}
