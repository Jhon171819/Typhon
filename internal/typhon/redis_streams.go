package typhon

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	maxStreamPayload = 16 << 10
	maxStreamBatch   = 64
)

const redisStreamClaim = `local claimed = redis.call('XAUTOCLAIM', KEYS[1], ARGV[1], ARGV[2], ARGV[3], ARGV[4], 'COUNT', ARGV[5])
local messages = {}
for _, message in ipairs(claimed[2]) do
  local id = message[1]
  local pending = redis.call('XPENDING', KEYS[1], ARGV[1], id, id, 1)
  if #pending > 0 then
    local payload = ''
    for i = 1, #message[2], 2 do
      if message[2][i] == 'payload' then payload = message[2][i + 1] end
    end
    if string.len(payload) > tonumber(ARGV[6]) then payload = '' end
    table.insert(messages, {id, payload, pending[1][4]})
  end
end
return {claimed[1], messages, #(claimed[3] or {})}`

const redisStreamFinish = `local pending = redis.call('XPENDING', KEYS[1], ARGV[1], ARGV[2], ARGV[2], 1)
if #pending == 0 or pending[1][2] ~= ARGV[3] or pending[1][4] ~= tonumber(ARGV[4]) then
  return 0
end
if ARGV[5] ~= '' then
  redis.call('XADD', KEYS[2], 'MAXLEN', ARGV[6], '*', 'source_id', ARGV[2], 'reason', ARGV[5], 'attempts', ARGV[4], 'payload', ARGV[7])
end
return redis.call('XACK', KEYS[1], ARGV[1], ARGV[2])`

type redisStream struct {
	db         *redisConnection
	key, dead  string
	group      string
	maxEntries int64
}

type redisDelivery struct {
	stream       redisStream
	id, consumer string
	payload      string
	attempt      int64
	started      time.Time
}

func redisStreamTypes() (Type, Type, Type) {
	delivery := Type{Kind: TypeNative, Name: "typhon.redis.Delivery", Members: map[string]Type{
		"id": StrType(), "attempt": IntType(), "payload": StrType(),
		"decode":      nativeFuncType("redis.delivery.decode", nil, AnyType(), 0),
		"ack":         nativeFuncType("redis.delivery.ack", nil, BoolType(), 0),
		"dead_letter": nativeFuncType("redis.delivery.dead_letter", []Type{StrType()}, BoolType(), 0),
	}}
	claim := Type{Kind: TypeNative, Name: "typhon.redis.Claim", Members: map[string]Type{
		"next": StrType(), "messages": listType(delivery), "trimmed": IntType(),
	}}
	stream := Type{Kind: TypeNative, Name: "typhon.redis.Stream", Members: map[string]Type{
		"create_group": nativeFuncType("redis.stream.create_group", nil, NoneType(), 0),
		"publish":      nativeFuncType("redis.stream.publish", []Type{AnyType()}, StrType(), 0),
		"read":         nativeFuncType("redis.stream.read", []Type{StrType(), IntType(), IntType()}, listType(delivery), 1),
		"claim":        nativeFuncType("redis.stream.claim", []Type{StrType(), IntType(), StrType(), IntType()}, claim, 0),
		"stats":        nativeFuncType("redis.stream.stats", nil, dictType(StrType(), IntType()), 0),
	}}
	return stream, delivery, claim
}

func (db *redisConnection) newStream(vm *VM, args []Value) (Value, error) {
	name, group := args[0].Str, args[1].Str
	key := "typhon:stream:{" + name + "}"
	stream := redisStream{db: db, key: key, dead: key + ":dlq:" + group, group: group, maxEntries: 10000}
	if len(args) == 3 {
		stream.maxEntries = args[2].Int
	}
	if !validIdempotencyKey(name) || !validIdempotencyKey(group) || stream.maxEntries < 1 || stream.maxEntries > 100000 {
		return NoneValue(), errors.New("Redis stream requires portable names and retention between 1 and 100000 entries")
	}
	typ, _, _ := redisStreamTypes()
	return scopedNativeObject(vm, typ, map[string]Value{
		"create_group": driverFunc(typ.Members["create_group"], stream.createGroup),
		"publish":      driverFunc(typ.Members["publish"], stream.publish),
		"read":         driverFunc(typ.Members["read"], stream.read),
		"claim":        driverFunc(typ.Members["claim"], stream.claim),
		"stats":        driverFunc(typ.Members["stats"], stream.stats),
	}), nil
}

func (s redisStream) createGroup(vm *VM, _ []Value) (Value, error) {
	return NoneValue(), s.db.run(vm.context(), s.db.timeout, func(ctx context.Context) error {
		err := s.db.client.XGroupCreateMkStream(ctx, s.key, s.group, "0-0").Err()
		if err != nil && redis.HasErrorPrefix(err, "BUSYGROUP") {
			return nil
		}
		return err
	})
}

func (s redisStream) publish(vm *VM, args []Value) (Value, error) {
	payload, err := dumpJSON(vm, args[0])
	if err != nil {
		return NoneValue(), err
	}
	if len(payload) > maxStreamPayload {
		return NoneValue(), errors.New("Redis stream JSON payload exceeds 16 KiB")
	}
	var id string
	err = s.db.run(vm.context(), s.db.timeout, func(ctx context.Context) error {
		var err error
		id, err = s.db.client.XAdd(ctx, &redis.XAddArgs{
			Stream: s.key, MaxLen: s.maxEntries, Values: []string{"payload", payload},
		}).Result()
		return err
	})
	return StringValue(id), err
}

func (s redisStream) read(vm *VM, args []Value) (Value, error) {
	consumer, count := args[0].Str, args[1].Int
	var blockMillis int64
	if len(args) == 3 {
		blockMillis = args[2].Int
	}
	if !validIdempotencyKey(consumer) || count < 1 || count > maxStreamBatch ||
		blockMillis < 0 || blockMillis > 5000 || (vm.request != nil && blockMillis > 0) ||
		(blockMillis > 0 && time.Duration(blockMillis)*time.Millisecond >= s.db.timeout) {
		return NoneValue(), errors.New("Redis stream read requires a consumer, 1 to 64 messages and a block shorter than the driver timeout, up to 5000 ms outside HTTP")
	}
	block := time.Duration(-1)
	if blockMillis != 0 {
		block = time.Duration(blockMillis) * time.Millisecond
	}
	var batches []redis.XStream
	err := s.db.run(vm.context(), s.db.timeout, func(ctx context.Context) error {
		var err error
		batches, err = s.db.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Streams: []string{s.key, ">"}, Group: s.group, Consumer: consumer, Count: count, Block: block,
		}).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	})
	if err != nil {
		return NoneValue(), err
	}
	result := make([]Value, 0, count)
	for _, batch := range batches {
		for _, message := range batch.Messages {
			payload, _ := message.Values["payload"].(string)
			value, err := s.delivery(vm, message.ID, consumer, payload, 1)
			if err != nil {
				return NoneValue(), err
			}
			result = append(result, value)
		}
	}
	return ListValue(result), nil
}

func validStreamID(id string) bool {
	first, second, ok := strings.Cut(id, "-")
	if !ok || len(first) > 20 || len(second) > 20 {
		return false
	}
	_, firstErr := strconv.ParseUint(first, 10, 64)
	_, secondErr := strconv.ParseUint(second, 10, 64)
	return firstErr == nil && secondErr == nil
}

func (s redisStream) claim(vm *VM, args []Value) (Value, error) {
	consumer, idle, start, count := args[0].Str, args[1].Int, args[2].Str, args[3].Int
	if !validIdempotencyKey(consumer) || idle < 1 || idle > 3600000 || !validStreamID(start) || count < 1 || count > maxStreamBatch {
		return NoneValue(), errors.New("Redis stream claim requires a consumer, idle time of 1 to 3600000 ms, a stream ID cursor and 1 to 64 messages")
	}
	var raw []any
	err := s.db.run(vm.context(), s.db.timeout, func(ctx context.Context) error {
		var err error
		raw, err = s.db.client.Eval(ctx, redisStreamClaim, []string{s.key}, s.group, consumer, idle, start, count, maxStreamPayload).Slice()
		return err
	})
	if err != nil {
		return NoneValue(), err
	}
	if len(raw) != 3 {
		return NoneValue(), errors.New("invalid Redis stream claim response")
	}
	cursor, cursorOK := raw[0].(string)
	messages, messagesOK := raw[1].([]any)
	trimmed, trimmedOK := raw[2].(int64)
	if !cursorOK || !validStreamID(cursor) || !messagesOK || len(messages) > int(count) || !trimmedOK {
		return NoneValue(), errors.New("invalid Redis stream claim response")
	}
	result := make([]Value, 0, len(messages))
	for _, reply := range messages {
		entry, ok := reply.([]any)
		if !ok || len(entry) != 3 {
			return NoneValue(), errors.New("invalid Redis stream delivery")
		}
		id, idOK := entry[0].(string)
		payload, payloadOK := entry[1].(string)
		attempt, attemptOK := entry[2].(int64)
		if !idOK || !payloadOK || !attemptOK || attempt < 1 || !validStreamID(id) {
			return NoneValue(), errors.New("invalid Redis stream delivery")
		}
		value, err := s.delivery(vm, id, consumer, payload, attempt)
		if err != nil {
			return NoneValue(), err
		}
		result = append(result, value)
	}
	s.db.streamTrimmedPending += trimmed
	_, _, typ := redisStreamTypes()
	return scopedNativeObject(vm, typ, map[string]Value{
		"next": StringValue(cursor), "messages": ListValue(result), "trimmed": IntValue(trimmed),
	}), nil
}

func (s redisStream) delivery(vm *VM, id, consumer, payload string, attempt int64) (Value, error) {
	if len(payload) > maxStreamPayload {
		payload = ""
	}
	if err := vm.chargeBytes(int64(len(payload)+len(id)) + 6*valueSlotBytes); err != nil {
		return NoneValue(), err
	}
	d := redisDelivery{stream: s, id: id, consumer: consumer, payload: payload, attempt: attempt, started: time.Now()}
	_, typ, _ := redisStreamTypes()
	return scopedNativeObject(vm, typ, map[string]Value{
		"id": StringValue(id), "payload": StringValue(payload), "attempt": IntValue(attempt),
		"decode": driverFunc(typ.Members["decode"], func(vm *VM, _ []Value) (Value, error) {
			return loadJSON(vm, payload)
		}),
		"ack": driverFunc(typ.Members["ack"], func(vm *VM, _ []Value) (Value, error) {
			return d.finish(vm, "")
		}),
		"dead_letter": driverFunc(typ.Members["dead_letter"], func(vm *VM, args []Value) (Value, error) {
			if !validIdempotencyKey(args[0].Str) {
				return NoneValue(), errors.New("Redis dead-letter reason must be a bounded diagnostic code")
			}
			return d.finish(vm, args[0].Str)
		}),
	}), nil
}

func (d redisDelivery) finish(vm *VM, reason string) (Value, error) {
	s := d.stream
	var ack int64
	err := s.db.run(vm.context(), s.db.timeout, func(ctx context.Context) error {
		var err error
		ack, err = s.db.client.Eval(ctx, redisStreamFinish, []string{s.key, s.dead},
			s.group, d.id, d.consumer, d.attempt, reason, s.maxEntries, d.payload).Int64()
		return err
	})
	if err == nil && ack == 1 {
		s.db.streamProcessed++
		s.db.streamProcessingNS += time.Since(d.started).Nanoseconds()
		if reason != "" {
			s.db.streamDeadLetters++
		}
	}
	return BoolValue(ack == 1), err
}

func (s redisStream) stats(vm *VM, _ []Value) (Value, error) {
	var pending *redis.XPending
	var groups []redis.XInfoGroup
	err := s.db.run(vm.context(), s.db.timeout, func(ctx context.Context) error {
		pipe := s.db.client.Pipeline()
		p := pipe.XPending(ctx, s.key, s.group)
		g := pipe.XInfoGroups(ctx, s.key)
		_, err := pipe.Exec(ctx)
		if err == nil {
			pending, groups = p.Val(), g.Val()
		}
		return err
	})
	if err != nil {
		return NoneValue(), err
	}
	lag := int64(-1)
	for _, group := range groups {
		if group.Name == s.group {
			lag = group.Lag
			break
		}
	}
	result := DictValue()
	_ = result.Dict.put(StringValue("pending"), IntValue(pending.Count))
	_ = result.Dict.put(StringValue("lag"), IntValue(lag))
	return result, nil
}
