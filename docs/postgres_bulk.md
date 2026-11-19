# TYP-025 — COPY e escrita em lote

`db.copy_from(table_segments, columns, rows)` usa o protocolo
binário COPY FROM STDIN. Passe o nome da tabela como
`['table']` ou `['schema', 'table']` e as colunas como lista
de nomes simples. O driver valida os identificadores, cita os
nomes com pgx e envia os valores por binding. Mantenha a escolha
de tabela e colunas no código confiável.

`db.exec_many(sql, rows)` envia um único modelo parametrizado
INSERT, UPDATE, DELETE ou MERGE por lote pgx. Repete somente
esse modelo para cada linha. O resultado é a soma das linhas
afetadas. Para adicionar dados em grandes volumes, use COPY.
Para upsert, use um modelo INSERT com ON CONFLICT no batch.
Os dois métodos devolvem zero para lote vazio.

Uma chamada aceita até 4.096 linhas, 128 valores por linha e
8 MiB de dados e peso de alocação. Ao ler um dataset maior,
processe lotes sucessivos de uma fonte, como
[`cursor.batch()`](postgres_cursor.md). Use conexões separadas
para ler e gravar ao mesmo tempo. Passe JSONB, UUID, Decimal
e outros valores com os [tipos de parâmetro](postgres_parameters.md).

Fora de transação explícita, cada chamada é atômica no PostgreSQL.
Se o processo enviar vários lotes separados, cada um pode ser
confirmado de forma independente. Use `tx.copy_from` e
`tx.exec_many` dentro de uma única transação quando vários lotes
devem ser confirmados juntos. O primeiro erro do lote faz rollback
da transação aberta e o driver devolve o erro original. A confirmação
da transação permanece explícita.

O [exemplo Typhon](../examples/postgres_bulk.ty) roda com
`DATABASE_URL` e esta tabela vazia de teste.

```sql
CREATE TABLE incoming (
  id bigint PRIMARY KEY,
  external uuid,
  payload jsonb,
  label text
);
```

O benchmark mede o trecho de envio ao PostgreSQL de um lote de
1.000 linhas preparadas em memória. Compara INSERT serial com
uma transação envolvendo o lote, batch pgx e COPY. Usa uma
tabela temporária, com truncamento fora do tempo registrado.
O CI executa o teste com PostgreSQL 16 e publica linhas/s,
bytes/op e alocações/op no log.

```sh
go test ./internal/typhon -run '^$' -bench '^BenchmarkNativePostgresBulk$' -benchmem -benchtime=3x
```

[Referência COPY do PostgreSQL](https://www.postgresql.org/docs/16/sql-copy.html)
