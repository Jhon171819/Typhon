# TYP-022 — pooling PostgreSQL por grupo de workers

Use `pg.connect_pooled(dsn, options)` na inicialização do módulo.
`options` é um `dict[str, Any]`. Os métodos e parâmetros de consulta
da conexão continuam os mesmos. As APIs `pg.connect` e
`pg.connect_typed` mantêm suas assinaturas e seus pools por Engine.

| Opção | Padrão | Comportamento |
| --- | --- | --- |
| `share` | ausente | nome do pool físico compartilhado pelos workers da mesma instância |
| `max_connections` | 16 | máximo de conexões físicas, de 1 a 64 |
| `min_connections` | 0 | alvo de conexões, limitado pelo máximo |
| `idle_seconds` | 1800 | prazo de ociosidade |
| `lifetime_seconds` | 3600 | vida útil antes de substituição da conexão |
| `health_seconds` | 60 | intervalo de manutenção do pool |
| `connect_seconds` | 10 | prazo de abertura e probe inicial |
| `acquire_seconds` | 10 | espera máxima por conexão |
| `query_seconds` | 10 | prazo da operação depois de obter conexão |
| `grace_seconds` | 0.5 | janela para as transações do handle terminarem, até 2 s |
| `typed` | `False` | tipos de resultado de `pg.connect_typed` quando `True` |
| `require_tls` | `False` | exige verificação de certificado e hostname em cada destino |

Um nome compartilhado exige a mesma configuração e o mesmo DSN no
grupo. Cada Engine mantém o próprio handle e as próprias transações.
Conexões físicas podem ser reutilizadas por handles diferentes.
Engines independentes e processos distintos têm registries próprios.
Sem `share`, cada Engine cria seu próprio pool. O [exemplo](../examples/postgres_pool.ty)
usa três workers sobre um pool de quatro conexões.

Reserve no PostgreSQL o produto de réplicas, pools físicos por réplica
e máximo por pool, além das sessões de migração e manutenção.
Quando os pools têm tamanhos diferentes, use a soma dos máximos.
Para saturação, acompanhe `db.stats()` com `opened`, `idle`, `in_use`,
`waited_acquisitions`, `wait_ns` e `cancelled_acquisitions`.
Escalonação de filas deve considerar também o prazo da requisição.

O probe inicial falha quando os destinos estão indisponíveis.
Configure os destinos na ordem desejada via DSN e o TLS com
`sslmode=verify-full` quando exigido. Falhas de consulta ou escrita
não são repetidas automaticamente. O pool substitui conexões
inválidas quando necessário. Guarde DSN e certificados fora da
árvore versionada.

Ao chamar `close()`, o handle deixa de admitir novas operações.
Transações existentes têm a janela configurada, limitada pelo
orçamento de fechamento. O runtime cancela e faz rollback das
restantes e libera a referência ao pool. O último handle fecha o
pool físico. Durante shutdown do servidor, o contexto também
interrompe requisições ativas. O fechamento físico continua sujeito
ao prazo de destruição de conexões do pgx.
