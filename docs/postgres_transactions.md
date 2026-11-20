# TYP-023 — transações explícitas e retry

`db.begin(isolation?, read_only?)` continua abrindo uma transação
normal. `db.transaction(callback, isolation?, read_only?, max_retries?)`
delimita a transação à execução do callback `(pg.Transaction) -> Any`.
Chame `tx.commit()` explicitamente quando todas as operações tiverem
sido aceitas. Um callback que retorna com a transação aberta faz
rollback e pode devolver o valor calculado. Erro, timeout ou
cancelamento também encerram qualquer transação aberta. Uma falha
de rollback é registrada com o erro original preservado.

Os níveis são `read_committed`, `repeatable_read` e `serializable`.
Ao omitir o nível, o driver usa o padrão do servidor. Passe `True`
como segundo argumento de `begin` ou terceiro de `transaction`
para abrir em modo somente leitura. Para um escopo aninhado na
mesma transação, use savepoints com nomes fixos no SQL.
Dentro de um callback use o handle `tx` para os comandos da unidade.

Por padrão cada unidade roda uma vez. Informe de 1 a 4 em
`max_retries` para aceitar repetição apenas após `40001` ou `40P01`.
Cada tentativa começa outra transação após rollback, com espera
exponencial curta e jitter, sujeitos ao contexto da VM. A opção só
serve para unidades idempotentes. Falhas após confirmação, resposta
de commit ambígua, erros de integridade e timeout não disparam
retry. Quando a conexão ou o prazo se perde durante commit, o
driver sinaliza resultado desconhecido. Reconcilie o estado por
uma chave de operação antes de reenviar qualquer escrita.

Mantenha chamadas HTTP, Redis e outros efeitos externos fora do
callback. Para publicar um evento, grave a linha de outbox junto
com os dados da transação e publique depois de confirmar.
Job assíncrono ou escrita externa iniciados pelo callback não
participam do rollback.

O [exemplo](../examples/postgres_transaction_scope.ty) usa
PostgreSQL com `DATABASE_URL` configurada e esta tabela de teste.

```sql
CREATE TABLE members (email text PRIMARY KEY);
```
