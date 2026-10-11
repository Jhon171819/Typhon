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
| `http.json(status, value)` | `http.Response`; JSON de escalares, listas e dicionários com Content-Type apropriado |
| `http.serve(address, handler)` | `None`; bloqueia até cancelamento/encerramento |

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
`query(name)` (`str`). Header/query ausente retorna `''`. Não há streaming,
WebSocket ou upload multipart nesta API; o corpo completo fica em memória,
com limite de 8 MiB para requests e responses. Headers fornecidos são pares
`[nome, valor]`; nomes inválidos e quebras de linha são rejeitados.

O servidor recebe até 16 requisições em processamento/fila e retorna 503 quando
esse limite é atingido. Cada callback tem contexto de 10 segundos, orçamento de
10 milhões de instruções e limpeza de recursos próprios ao terminar. Uma Engine
serializa a execução Typhon; I/O feito pelo callback também ocupa essa execução.
Há prazos de leitura de headers (5s), leitura (10s), escrita (15s) e idle (60s).
Erros de callback são registrados no stderr e retornam 500; a próxima requisição
pode ser atendida. Ctrl+C cancela o contexto da CLI e fecha o servidor.

## PostgreSQL

Implementado com [`pgx/v5`](https://github.com/jackc/pgx), com pool de até
16 conexões. A conexão é validada por `Ping` ao abrir.

| Função/método | Retorno e comportamento |
| --- | --- |
| `pg.connect(dsn, timeout=10)` | `pg.Connection`; URL PostgreSQL ou DSN pgx, prazo por operação |
| `db.exec(sql, params=[])` | `int`; linhas afetadas |
| `db.query(sql, params=[])` | `pg.Rows`; resultado materializado |
| `db.begin()` | `pg.Transaction`; reserva uma conexão do pool |
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
O transporte de inteiros preserva int64; resultados `numeric` tornam-se strings
para preservar a precisão decimal. Texto, booleanos, inteiros e floats finitos
tornam-se seus tipos Typhon; datas/timestamps tornam-se strings RFC3339Nano,
UUID torna-se string e arrays tornam-se listas. `bytea` torna-se `str` com os bytes
originais. Objetos JSON retornados pelo PostgreSQL tornam-se dicionários,
com chaves ordenadas; arrays JSON tornam-se listas. Para outros tipos sem
representação, use cast SQL para `text`. Arrays como parâmetros ainda não são suportados.

`commit` confirma apenas quando chamado explicitamente. Transações abandonadas
fazem rollback ao encerrar a Engine; as abertas dentro de handlers HTTP fazem
rollback ao terminar a requisição, inclusive em erros. Fechamento e rollback são
idempotentes. Uso após fechamento/commit/rollback gera erro. Não há `try/except`
na linguagem ainda: fora do HTTP, um erro de driver encerra o programa e limpa
os recursos.

## Redis

Implementado com [`go-redis/v9`](https://github.com/redis/go-redis), RESP2,
pool de 16 conexões e `Ping` ao abrir. Usa `redis://` ou `rediss://` (TLS),
incluindo credenciais e número do banco na URL. Não repete automaticamente
operações que falharam, evitando duplicar uma escrita com resultado incerto.

| Função/método | Retorno e comportamento |
| --- | --- |
| `redis.connect(url, timeout=10)` | `redis.Connection`; prazo por operação |
| `cache.ping()` | `str`; normalmente `PONG` |
| `cache.get(key)` | `str \| None`; `None` quando a chave não existe |
| `cache.set(key, value, ttl=0)` | `None`; chave/valor `str`, TTL em segundos, zero sem expiração |
| `cache.delete(key)` | `int`; número de chaves removidas (0 ou 1) |
| `cache.exists(key)` | `bool` |
| `cache.incr(key)` | `int`; incremento atômico no Redis |
| `cache.ttl(key)` | `float`; segundos restantes, `-1` sem TTL, `-2` ausente |
| `cache.command(args)` | `Any`; comando único com `list[str]` |
| `cache.close()` | `None`; idempotente, impede novas chamadas |

TTL positivo mínimo: 0.001 segundos. Respostas de `command` tornam-se escalares,
`None` ou listas recursivas. Permite, por exemplo, hashes e listas com `HSET`,
`HGETALL`, `LPUSH` e `LRANGE`. Comandos que alteram o estado da conexão
(`MULTI`, `WATCH`, `SUBSCRIBE`, `AUTH`, `SELECT`, etc.) são rejeitados porque
chamadas independentes usam um pool. Não há API dedicada de transações, Pub/Sub,
Sentinel ou cluster nesta versão.

## Executar e testar

Todos os drivers usam timeout padrão de 10 segundos, máximo de 300 segundos,
e respeitam o contexto da Engine/requisição. Clientes, pools e transações
pertencem à Engine; recursos criados dentro de um callback HTTP pertencem
àquela requisição. Compartilhe clientes criados antes de `serve` e feche-os
quando não precisar mais deles. O host fecha os recursos restantes ao encerrar.

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
