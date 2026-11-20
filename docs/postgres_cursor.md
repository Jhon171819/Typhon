# TYP-024 — cursor PostgreSQL e leitura incremental

`db.cursor(sql, params?, fetch_size?)` e `tx.cursor(...)` abrem um
`pg.Cursor`. O pgx lê o fluxo do PostgreSQL sem materializar todo
o resultado. A conexão permanece reservada até o fim, `close()`
ou encerramento do escopo. Faça as consultas de relatório com
ordem explícita quando o consumidor depender dela.

`cursor.next()` retorna a próxima linha ou `None`. Use
`cursor.batch()` para receber até `fetch_size` linhas. Cada
lote tem até 4 MiB de dados brutos e peso de colunas, e pode
terminar antes de `fetch_size`. O próximo lote continua no ponto
exato em que o anterior parou. O lote vazio indica EOF.
`fetch_size` padrão é 128 e aceita de 1 a 1.024. Esse tamanho
controla a materialização no cliente pgx/Typhon.

`cursor.columns` lista os nomes das colunas. Os valores seguem as
regras de `db.query` ou `pg.connect_typed`, conforme a conexão.
Consultas que materializam resultados continuam usando `db.query()`.
Um cursor vinculado ao escopo de uma requisição ou transação não
pode ser transferido para outra execução.

Use `finally` com `cursor.close()` ao sair antecipadamente.
EOF libera o cursor automaticamente. Uma transação não executa
outro comando enquanto seu cursor está ativo. Depois de terminar
a leitura, a transação pode continuar. Quando um cursor
transacional falha ou é fechado antes de EOF, faça rollback.
O driver pode descartar a conexão física ao interromper a leitura.
O pool fica livre para fornecer uma conexão válida.

Cada avanço respeita os prazos de consulta e execução e os
orçamentos de bytes e instruções. `TYPHON_LIMIT_SQL_CURSOR_ROWS`
permite um teto dedicado de até 10 milhões, quando configurado.
Sem ele, vale o orçamento de linhas SQL existente de
`TYPHON_LIMIT_SQL_ROWS`. O novo teto não aumenta o limite
de materialização de `db.query`.

O [exemplo de exportação JSONL](../examples/postgres_cursor_export.ty)
envia até 100 mil linhas por HTTP. Configure `DATABASE_URL` e um
orçamento de requisição apropriado ao volume e ao tempo de exportação.
Para executar o exemplo, use `TYPHON_LIMIT_REQUEST_BYTES=134217728`
e faça GET em `/v1/report`.
Se o cliente desconectar, a VM fecha o cursor e encerra a transação.
Para rotinas Python, passe lotes de dados serializáveis pelo bridge
quando necessário e mantenha o cursor no runtime Go.
