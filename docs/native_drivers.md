# Drivers nativos em Go

`typhon.http`, `typhon.postgres` e `typhon.redis` executam dentro da VM Go,
sem iniciar Python. Seus tipos, atributos, argumentos e retornos são verificados
pelo checker e novamente nas fronteiras de execução, inclusive quando se usa `Any`.
Use Go 1.26 ou mais recente para compilar.

```typhon
import typhon.http as http
import typhon.postgres as pg
import typhon.redis as redis

# Também é possível importar os tipos e funções diretamente:
from typhon.http import Request, Response, response, serve
```

Os tipos nativos são opacos; crie instâncias pelas funções do módulo.
As chamadas aceitam argumentos posicionais. `env(nome)` existe nos três módulos
e retorna o valor da variável de ambiente, ou `''` quando ela não está definida.

## HTTP

Implementado com [`net/http`](https://pkg.go.dev/net/http).

| Função/método | Retorno e comportamento |
| --- | --- |
| `http.client(timeout=10)` | `http.Client`; prazo em segundos por requisição |
| `client.get(url)` | `http.Response`; GET HTTP ou HTTPS |
| `client.request(method, url, body, headers=[])` | `http.Response`; corpo `str`, headers `list[list[str]]` |
| `client.close()` | `None`; fecha conexões ociosas e impede novas chamadas |
| `http.response(status, body, headers=[])` | `http.Response`; status de 200 a 599 |
| `http.json(status, value)` | `http.Response`; [JSON nativo](native_json.md) com Content-Type apropriado |
| `http.serve(address, handler, workers=1, queue=15, request_timeout=10)` | `None`; aceita função ou [router](http_router.md) e serve até o encerramento |
| `http.server_stats()` | `dict[str, int]`; métricas de admissão e espera do servidor ativo |

O `=` nas tabelas indica o valor padrão; na chamada Typhon, passe valores
posicionais: `http.client(5)`, `http.response(200, 'ok', [['X-App', 'Typhon']])`.

`http.Response` oferece `status: int`, `body: str` e `header(name: str) -> str`.
O cliente verifica certificados HTTPS e segue redirecionamentos segundo as regras
do cliente Go. Códigos 4xx/5xx são respostas normais; falhas de transporte são erros.

O handler deve ser uma função Typhon `(http.Request) -> http.Response`:

```typhon
import typhon.http as http

def handle(req: http.Request) -> http.Response:
    if req.path == '/health':
        return http.response(200, 'ok')
    return http.json(200, [req.method, req.path, req.query('name'), req.body])

http.serve('127.0.0.1:8080', handle)
```

`http.Request` oferece `method`, `path`, `body` (`str`), `header(name)` e
`query(name)` (`str`). Header/query ausente retorna `''`. O corpo
fica em memória por padrão, com limite de 8 MiB para requests e
responses. Use [streaming HTTP](http_streaming.md) para multipart e
respostas por blocos. Headers fornecidos são pares
`[nome, valor]`; nomes inválidos e quebras de linha são rejeitados.
`req.json()` decodifica o corpo com o mesmo codec de `json.loads`.
`req.query_all(name)` devolve todas as ocorrências, e `req.param(name)`
expõe os parâmetros do caminho de um router.
O [orçamento lógico](request_budgets.md) pode impor um teto menor e
também considera bytes materializados pelo PostgreSQL e os corpos HTTP.

Com a configuração padrão o servidor recebe até 16 requisições em
processamento/fila e retorna 503 quando esse limite é atingido. Cada
callback tem contexto de 10 segundos, orçamento de 10 milhões de instruções
e limpeza de recursos próprios ao terminar. Aumente o número de
[workers isolados](http_workers.md) quando precisar avançar requisições
simultâneas. O [prazo da requisição](request_context.md) começa na
admissão e cobre fila, leitura e execução. `req.remaining()` e
`req.cancelled()` inspecionam o contexto original.
Há prazos de leitura de headers (5s), leitura (10s), escrita (15s) e idle (60s).
Falhas de callback são registradas no stderr. A próxima requisição pode
ser atendida. Ctrl+C e SIGTERM cancelam o contexto da CLI e fecham o
servidor.

## PostgreSQL

Implementado com [`pgx/v5`](https://github.com/jackc/pgx), com pool de até
16 conexões. A conexão é validada por `Ping` ao abrir.

| Função/método | Retorno e comportamento |
| --- | --- |
| `pg.connect(dsn, timeout=10)` | `pg.Connection`; URL PostgreSQL ou DSN pgx, prazo por operação |
| `pg.connect_typed(dsn, timeout=10)` | `pg.Connection`; decodifica [escalares nativos](native_scalars.md) e JSON/JSONB com precisão |
| `db.exec(sql, params=[])` | `int`; linhas afetadas |
| `db.query(sql, params=[])` | `pg.Rows`; resultado materializado |
| `db.begin(isolation?, read_only?)` | `pg.Transaction`; reserva uma conexão do pool |
| `db.transaction(callback, isolation?, read_only?, max_retries?)` | `Any`; fecha a transação no fim do callback, com [commit explícito](postgres_transactions.md) |
| `tx.exec(sql, params=[])`, `tx.query(sql, params=[])` | Mesmos contratos da conexão |
| `tx.commit()`, `tx.rollback()` | `None`; encerra a transação |
| `db.close()` | `None`; faz rollback das transações abertas e fecha o pool |

Use parâmetros `$1`, `$2`, etc.; evite montar SQL com concatenação de dados:

```typhon
import typhon.postgres as pg

db: pg.Connection = pg.connect(pg.env('DATABASE_URL'), 5)
rows: pg.Rows = db.query('SELECT $1::bigint AS id, $2::text AS name', [9007199254740993, 'Ada'])
id: int = rows.get(0, 'id')
name: str = rows.get(0, 'name')
print(id, name)
db.close()
```

`pg.Rows` oferece `count: int`, `columns: list[str]`,
`get(row: int, column: str) -> Any` e `row(index: int) -> list[Any]`.
Índices começam em zero. Índice/coluna ausente gera erro. Use aliases SQL para
nomes únicos; em nomes duplicados, `get` seleciona a última coluna.
Resultados têm limite de 100 mil linhas; use `LIMIT`/paginação em consultas grandes.

Parâmetros aceitam `None`, `bool`, `int`, `float` finito e `str`.
Também aceitam os escalares de `typhon.scalars`.
O transporte de inteiros preserva int64; resultados `numeric` tornam-se strings
para preservar a precisão decimal. Texto, booleanos, inteiros e floats finitos
tornam-se seus tipos Typhon; datas/timestamps tornam-se strings RFC3339Nano,
UUID torna-se string e arrays tornam-se listas. `bytea` torna-se `str` com os bytes
originais. Objetos JSON retornados pelo PostgreSQL tornam-se dicionários,
com chaves ordenadas; arrays JSON tornam-se listas. Para outros tipos sem
representação, use cast SQL para `text`. Consulte [parâmetros PostgreSQL
e SQLSTATE](postgres_parameters.md) para arrays, JSONB e nulls explícitos.

`commit` confirma apenas quando chamado explicitamente. Transações abandonadas
fazem rollback ao encerrar a Engine; as abertas dentro de handlers HTTP fazem
rollback ao terminar a requisição, inclusive em erros. Fechamento e rollback são
idempotentes. Uso após fechamento/commit/rollback gera `ValidationError`.
Uma falha SQL capturada pode deixar a transação abortada. Use `rollback()`
para encerrá-la e continuar usando o pool.

O [contrato de limpeza](resource_cleanup.md) descreve a ordem LIFO, os
prazos e a propriedade de métodos e recursos por requisição.

## Redis

Implementado com [`go-redis/v9`](https://github.com/redis/go-redis), RESP2,
pool de 16 conexões e `Ping` ao abrir. Usa `redis://` ou `rediss://` (TLS),
incluindo credenciais e número do banco na URL. Não repete automaticamente
operações que falharam, evitando duplicar uma escrita com resultado incerto.

| Função/método | Retorno e comportamento |
| --- | --- |
| `redis.connect(url, timeout=10)` | `redis.Connection`; prazo por operação |
| `redis.connect_with_pool(url, pool_size, min_idle, dial=10, read=10, write=10, probe=True)` | `redis.Connection`; configuração por Engine, de 1 a 64 conexões |
| `cache.ping()` | `str`; normalmente `PONG` |
| `cache.get(key)` | `str \| None`; `None` quando a chave não existe |
| `cache.set(key, value, ttl=0)` | `None`; chave/valor `str`, TTL em segundos, zero sem expiração |
| `cache.delete(key)` | `int`; número de chaves removidas (0 ou 1) |
| `cache.exists(key)` | `bool` |
| `cache.incr(key)` | `int`; incremento atômico no Redis |
| `cache.ttl(key)` | `float`; segundos restantes, `-1` sem TTL, `-2` ausente |
| `cache.command(args)` | `Any`; comando único com `list[str]` |
| `cache.pipeline(commands)` | `list[redis.Result]`; lote ordenado de até 1.000 comandos com `list[list[str]]` |
| `cache.mget(keys)`, `cache.mset(pairs)` | `list[str \| None]` na ordem das chaves ou `None` para `dict[str, str]` |
| `cache.hget(key, field)`, `cache.hset(key, field, value)` | `str \| None` e `int` (campos recém-criados) |
| `cache.stats()` | `dict[str, int]`; operações, latência acumulada em ns, timeouts, falhas de rede e conexões |
| `cache.close()` | `None`; idempotente, impede novas chamadas |

TTL positivo mínimo: 0.001 segundos. Respostas de `command` tornam-se escalares,
`None` ou listas recursivas. Permite, por exemplo, hashes e listas com `HSET`,
`HGETALL`, `LPUSH` e `LRANGE`. Comandos que alteram o estado da conexão
(`MULTI`, `WATCH`, `SUBSCRIBE`, `AUTH`, `SELECT`, etc.) são rejeitados porque
chamadas independentes usam um pool. Não há API dedicada de transações, Pub/Sub,
Sentinel ou cluster nesta versão.

Use `connect_with_pool` para separar prazos de conexão, leitura e escrita.
O prazo total de uma operação é o maior desses valores, limitado também pelo
contexto da requisição. `min_idle` é um alvo do pool, menor ou igual ao
tamanho configurado. Cada worker tem seu próprio pool. Forneça URL com ACL
(`rediss://usuário:senha@host:6379/0` em produção) por variável de ambiente.
O modo configurável exige verificação do certificado TLS. A VM sanitiza
erros e as métricas omitem URL, comandos e credenciais.
Passe `False` no argumento final e use `min_idle=0` para adiar a
conexão até a primeira operação.

O pipeline valida o lote antes do envio e agrupa os comandos de uso comum
para evitar uma espera de rede por comando. Aceita PING, GET, SET, DEL,
EXISTS, INCR, DECR, EXPIRE, TTL, PTTL, MGET, MSET, HGET, HSET, HEXISTS e HDEL,
com até 2 MiB de argumentos no lote. Cada resultado expõe `ok`, `value`
e `error`. Chave ausente é sucesso com `None`. Uma rejeição do Redis
produz `redis_error` naquele item. Uma falha de transporte sem resposta
confirmada produz `outcome_unknown`. O pipeline permite sucesso parcial
e as escritas podem ter ocorrido mesmo quando a resposta se perde.
Reenvie somente operações idempotentes após avaliar ou reconciliar o estado.
Veja [o exemplo Typhon](../examples/redis_pipeline.ty) e a
[semântica do pipeline no Redis](https://redis.io/docs/latest/develop/using-commands/pipelining/).

## Executar e testar

Todos os drivers usam timeout padrão de 10 segundos, máximo de 300 segundos,
e respeitam o contexto da Engine/requisição. Clientes, pools e transações
pertencem à Engine; recursos criados dentro de um callback HTTP pertencem
àquela requisição. Compartilhe clientes criados antes de `serve` e feche-os
quando não precisar mais deles. O host fecha os recursos restantes ao encerrar.

Falhas recuperáveis de driver podem ser tratadas com `except` tipado.
O [contrato de exceções](typed_exceptions.md) documenta o mapeamento para
`ValidationError`, `TimeoutError`, `ConnectionError` e `DatabaseError`,
o cancelamento do runtime e a cadeia sanitizada de erros.

```powershell
go build -o typhon.exe ./cmd/typhon
.\typhon.exe run examples\http_server.ty
# Em outro terminal:
.\typhon.exe run examples\http_client.ty

$env:DATABASE_URL = 'postgres://usuario:senha@127.0.0.1:5432/banco?sslmode=disable'
.\typhon.exe run examples\postgres.ty
$env:REDIS_URL = 'redis://127.0.0.1:6379/0'
.\typhon.exe run examples\redis.ty

go test ./...
# Use instâncias exclusivas de teste:
$env:TYPHON_TEST_POSTGRES_URL = $env:DATABASE_URL
$env:TYPHON_TEST_REDIS_URL = $env:REDIS_URL
go test ./internal/typhon -run TestNative -count=1 -v
```

Os testes comuns cobrem HTTP real em loopback e Redis via servidor RESP
`miniredis`, além dos contratos estáticos/dinâmicos. Os testes de integração
usam as URLs acima; sem elas, são marcados como skipped. O workflow de CI inclui
PostgreSQL e Redis reais no Linux, com race detector.
