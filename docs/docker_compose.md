# Typhon API with Docker Compose

The development stack builds the Go VM and runs a [records API](../examples/compose/service.ty)
with PostgreSQL persistence and a Redis connectivity endpoint. Only the
API port is published, on loopback. The database and Redis use a private
backend network. PostgreSQL data lives in a named volume. Redis is
ephemeral.

Copy `.env.example` to `.env` and set `POSTGRES_PASSWORD` to a random local
password. Set `TYPHON_PORT` if port 8080 is in use. Then start the stack:

```sh
docker compose up --build -d
docker compose ps -a
curl -i http://127.0.0.1:8080/health/live
curl -i http://127.0.0.1:8080/health/ready
```

Compose waits for PostgreSQL health before running the one-shot migration.
The API starts after that migration succeeds and Redis starts accepting
connections. The container healthcheck measures liveness. Check readiness
separately before sending traffic. The API turns unready during
dependency failure, full admission or drain. [Compose startup order](https://docs.docker.com/compose/how-tos/startup-order/),
[Typhon health checks](http_health.md)

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"name":"Ada"}' http://127.0.0.1:8080/records/demo
curl http://127.0.0.1:8080/records/demo
curl -X PUT -H 'Content-Type: application/json' \
  -d '{"name":"Ada Lovelace"}' http://127.0.0.1:8080/records/demo
curl -X DELETE http://127.0.0.1:8080/records/demo
curl http://127.0.0.1:8080/cache
docker compose logs api db redis migrate
docker compose down
```

`docker compose down` retains the PostgreSQL volume for the next start.
`docker compose down --volumes` deletes that development data.
The API has a 3-second grace period and a 15-second container stop timeout.

The multi-stage image checks its Go version against `go.mod`, builds a
static binary with `-trimpath` and a fixed link configuration, and copies
only that binary and the reference app into Alpine with CA certificates.
It runs as UID and GID 10001 on a read-only root filesystem under Compose.
The image contains neither the Go toolchain nor a Python interpreter.
Environment variables supply development credentials at runtime. The
development stack uses one PostgreSQL login for migration and application
traffic. Use separate database roles and deployment-managed secrets for
a deployed service. [Docker multi-stage builds](https://docs.docker.com/build/building/multi-stage/)

Run `bash tests/compose_smoke.sh` with an isolated Docker engine and port
18081 available. The script builds the image, checks UID and CA certificates,
probes readiness and liveness, exercises CRUD and Redis, checks image history
for its generated password, stops the API and recreates the database container
to verify volume persistence. Its Compose project and volume are temporary.
