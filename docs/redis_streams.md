# Redis Streams workers

`redis_connection.stream(name, group, max_entries?)` binds a consumer
group. Names and consumer IDs have 1–128 ASCII letters, digits or
`-._~`. Use a distinct consumer ID for each worker process.
The source key is `typhon:stream:{name}`. Its group's dead-letter key
is `typhon:stream:{name}:dlq:group`.

| Operation | Contract |
| --- | --- |
| `create_group()` | Idempotent group creation, starting from the beginning of the stream. |
| `publish(value)` | Native JSON of up to 16 KiB. Returns the Redis stream ID acknowledged by `XADD`. |
| `read(consumer, count, block_ms?)` | Gives up to 64 new deliveries using `XREADGROUP`. Default zero returns immediately. A positive block is available in a standalone worker up to 5,000 ms, below the configured connection timeout. |
| `claim(consumer, idle_ms, cursor, count)` | Uses `XAUTOCLAIM` to recover idle deliveries. Follow `Claim.next` and wrap from `0-0`. `Claim.trimmed` reports pending IDs already evicted from the stream. |
| `stats()` | Group lag from Redis (`-1` when unknown) and undelivered acknowledgements from `XPENDING`. |
| `delivery.decode()` | Decodes the original JSON. Validate its schema version in the worker. |
| `delivery.ack()` | Confirms the current delivery using `XACK`. Returns `False` if its consumer or delivery attempt has changed. |
| `delivery.dead_letter(reason)` | Atomically checks ownership, appends the delivery to the DLQ and ACKs the source. Use a bounded error code. |

The `Delivery` also exposes its ID, one-based attempt and original
payload. Driver counters include cumulative processing time until ACK,
number of completed deliveries, dead letters and pending entries
discovered after trimming. The runtime does not include consumer IDs
or job IDs as metric labels.

## Producer and worker

The [HTTP producer](../examples/streams/producer.ty) validates a versioned
job and responds 202 after enqueue. The [worker](../examples/streams/worker.ty)
validates the same schema, persists a result by unique `job_id`, then
acknowledges the Redis delivery. It reclaims after a fixed five-second
idle delay and dead-letters on the third failed attempt. Invalid
schema or JSON is dead-lettered immediately. Set the reclaim delay
above the longest expected processing time in your worker.

Set `DATABASE_URL` for a development database, `REDIS_URL` for Redis 7
or later and `REPORT_STREAM` to the same name for both processes. Set a
unique `REPORT_CONSUMER` for each worker. Run the migration and the two
processes in separate terminals:

```sh
typhon migrate up examples/streams/migrations
typhon run examples/streams/producer.ty
typhon run examples/streams/worker.ty
```

```sh
curl -X POST http://127.0.0.1:8080/reports \
  -H 'Content-Type: application/json' \
  -d '{"version":1,"kind":"report","job_id":"quarter-03","title":"Quarterly report"}'
```

Use a unique, stable `job_id` for the lifetime of a logical job, including
its retries. Redis can redeliver after a worker crashes between
PostgreSQL commit and ACK. The database unique key makes that repeated
effect safe. The example binds to loopback for development.

## Retention and durable publishing

Both source and DLQ use bounded Redis retention (10,000 entries by
default and 1–100,000 configurable). Trimming removes the oldest entries,
including work still pending when the queue falls behind. Monitor
`lag`, `pending` and `stream_trimmed_pending`. Keep enough capacity for
outages, and store recoverable business data in PostgreSQL.

For jobs created by a database write, insert a stable job ID and payload
in an outbox table in the same transaction as the business change.
Publish the outbox record from a separate worker. Mark it sent only after
`XADD` succeeds. A crash after publication can publish the same record
again. Protect the consumer's state change with a unique job ID in its
database transaction, and ACK only after commit. Retain the outbox long
enough to restore events after Redis truncation or failover.
