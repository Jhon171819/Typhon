# TYP-021 — parâmetros PostgreSQL e SQLSTATE

Passe valores como parâmetros `$1`, `$2` e assim por diante.
`pg.array(type, values)` cria um array SQL unidimensional de até
4.096 elementos. Os nomes aceitos são `int4`, `int8`, `text`, `bool`,
`float8`, `numeric`, `uuid`, `date`, `timestamp`, `timestamptz` e
`bytea`. Use os [escalares Typhon](native_scalars.md) para tipos como
Decimal, UUID, datas e Bytes. `None` representa um elemento SQL nulo
e uma lista vazia representa um array vazio.

`pg.jsonb(value)` codifica o valor pelo codec JSON nativo. Um
dicionário passado diretamente também é codificado como JSONB.
Para lista no nível do parâmetro, escolha entre `pg.array` e
`pg.jsonb`. Objetos e listas aninhados são aceitos dentro de JSONB.
`pg.jsonb(None)` representa o valor JSON `null`. Use
`pg.null('jsonb')` ou `None` para SQL `NULL`; a variante tipada
também aceita tipos de array, como `pg.null('text[]')`.

O PostgreSQL determina o tipo esperado do parâmetro pelo contexto.
`WHERE id = ANY($1)`, `WHERE data @> $1` e `INSERT ... VALUES ($1)`
normalmente fornecem esse contexto. Em um `SELECT $1` isolado,
informe o tipo fixo na consulta, por exemplo `SELECT $1::uuid`.
Arrays multidimensionais e tipos SQL definidos pelo usuário ficam
fora do encoder atual. Parâmetros timestamp são limitados à precisão
de microssegundo do PostgreSQL.

O driver continua aceitando escalares posicionais existentes. Use
`pg.connect_typed` para obter escalares nativos nas colunas e preservar
a precisão de inteiros no JSONB retornado. Consulte o exemplo
[`postgres_parameters.ty`](../examples/postgres_parameters.ty).
Configure `DATABASE_URL` para executá-lo. O CI verifica PostgreSQL 16
com a versão do pgx fixada em `go.mod`.

Um erro do PostgreSQL pode fornecer `sqlstate`, `constraint`, `table`
e `column` no objeto de exceção. Esses atributos são `str | None`.
Os identificadores são expostos só quando vêm de diagnósticos de
integridade e atendem à sintaxe simples esperada. A mensagem do
servidor, detalhes com valores, caminhos internos e SQL completo
ficam fora da exceção pública. Use os campos em logs protegidos
e mantenha mensagens HTTP livres de metadados internos.

`23505` identifica conflito de unicidade, `23503` referência
inválida e `40P01` deadlock. Os três chegam como `DatabaseError`.
O cancelamento de comando no servidor com `57014`, incluindo
`statement_timeout`, chega como `TimeoutError` com o código
preservado. Um timeout local pode não trazer SQLSTATE.

```typhon
try:
    db.exec('INSERT INTO members (email) VALUES ($1)', [email])
except DatabaseError as err:
    if err.sqlstate == '23505':
        print('member already exists')
    else:
        raise
```

[Referência SQLSTATE do PostgreSQL](https://www.postgresql.org/docs/16/errcodes-appendix.html)
