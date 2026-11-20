# TYP-009 — escalares nativos

`typhon.scalars` fornece valores imutáveis com construtores explícitos.
O checker verifica tipos de argumentos e resultados. Dados malformados
retornam `ValidationError` com mensagens que omitem a entrada.

| Tipo | Construção e operações | JSON | PostgreSQL |
| --- | --- | --- | --- |
| `Bytes` | `bytes_utf8(text)`, `bytes_hex(text)`, `bytes_base64(text)`. Expõe `size`, `utf8()`, `hex()` e `base64()`. | String base64 padrão | `bytea` |
| `Decimal` | `decimal(text)`, `text`, `compare(other)` com resultado −1, 0 ou 1. | String decimal exata | `numeric` |
| `UUID` | `uuid(text)`, `text` em forma canônica minúscula | String canônica | `uuid` |
| `Date` | `date(text)`, `text` em `YYYY-MM-DD` | String ISO 8601 | `date` |
| `Timestamp` | `timestamp(text)`, `text` RFC3339 normalizado para UTC | String RFC3339 | `timestamptz` |
| `LocalTimestamp` | `local_timestamp(text)`, `text` sem fuso | String ISO 8601 | `timestamp` |

Bytes aceitam até 8 MiB decodificados, inclusive NUL. `utf8()`
valida os bytes antes de convertê-los em string. Hex aceita pares
hexadecimais, e base64 exige o alfabeto padrão com padding quando
necessário.

Decimal aceita sinal, dígitos e ponto decimal, com até 256
caracteres. O codec usa `pgtype.Numeric` para manter coeficiente e
expoente em inteiros e `math/big.Rat` para comparação exata. Compare
decimais com `compare()`. O tipo conserva a escala representável
no PostgreSQL e serializa texto, inclusive para quantias maiores
que o intervalo exato de float64.

Datas e timestamps aceitam anos de 0001 a 9999, sem valores
infinitos. Timestamp com fuso exige `Z` ou deslocamento numérico,
preserva o instante e normaliza para `Z`. Timestamp civil conserva
a hora local sem atribuição implícita de fuso. O codec aceita até
nove dígitos fracionários. Parâmetros PostgreSQL de timestamp
precisam ter precisão de microssegundos, para evitar truncamento.

`json.dumps()` e `http.json()` usam a coluna JSON da tabela.
`json.loads()` devolve strings, que podem ser passadas ao construtor
correspondente. Os formatos são explícitos e adequados a interfaces
que precisam preservar números grandes ou valores binários.

As duas formas de conexão aceitam escalares nativos como parâmetros
posicionais em consultas parametrizadas. `pg.connect(dsn, timeout)`
preserva as representações legadas nos resultados. Use
`pg.connect_typed(dsn, timeout)` para decodificar as colunas da tabela
em escalares nativos e JSON/JSONB com o codec Typhon, incluindo int64
exato. Outros tipos e arrays conservam a representação existente.

Execute [native_scalars.ty](../examples/native_scalars.ty) com
`DATABASE_URL` de teste. O exemplo usa tabela temporária dentro de
transação, parâmetros `$1` a `$7`, código binário com NUL, UUID,
numeric com 20 casas decimais, datas, dois tipos de timestamp e JSONB.
