# TYP-018 — paginação e filtros REST

Versões incompatíveis da API usam prefixo próprio na URL, como
`/v1/appointments` e `/v2/appointments`. Compartilhe o contrato de
paginação nas versões que preservam a ordenação e os filtros.

`web.paginator(key, order, filters, default_limit?, max_limit?, ttl_seconds?)`
configura um paginador reutilizável. A chave são 32 bytes aleatórios em
base64url sem padding. Leia-a de um segredo de ambiente e compartilhe o
mesmo valor entre as réplicas. A troca de chave invalida cursores já
emitidos. O TTL padrão é 900 segundos. Os limites padrão e máximo são
25 e 100. O teto configurável é 1.000 itens.

`pages.parse(req)` devolve `page.limit`, `page.offset`, `page.after` e
`page.order`. A ordem é um alias fixo, como `id_asc`. `page.after`
é zero sem cursor e guarda o último ID quando a próxima página é
solicitada. `offset` tem teto de 10.000 e pode ser usado na primeira
página. O próximo link usa a posição do cursor e zera o offset.
Configurar uma ordem diferente exige um novo contrato de consulta.

Use um `validation.object` fechado para declarar filtros obrigatórios
e opcionais. `page.filter(name)` devolve seu valor escalar ou `None`
quando ausente. Vincule os valores aos parâmetros SQL. Mantenha nomes de
colunas e a cláusula de ordenação no código versionado. As opções
`limit`, `offset`, `order` e `cursor` são reservadas. A query pode ter
até 8 KiB e cada valor até 1.024 bytes. Campos desconhecidos, parâmetros
duplicados, posição adulterada ou expirada e combinação de offset com
cursor geram 400 sob `routes.use_errors()`.

Consulte até `limit + 1` registros. Se houver um item extra, chame
`page.respond(items, last_id)` com os itens visíveis e seu último ID.
Na última página, chame `page.respond(items)`. A resposta JSON tem
`items`, `limit`, `next_cursor` e `links.next`. Use `links.next`
para preservar os parâmetros válidos. Um cursor autentica e cifra a
posição e é vinculado ao método, caminho, identidade, tenant, ordenação
e filtros. A mudança de `limit` ainda é permitida. Com Bearer, configure
a autenticação e a política na rota antes do handler de listagem.

O [exemplo PostgreSQL](../examples/http_paginated_appointments.ty) usa
`id > $2 ORDER BY id ASC` e inclui o tenant verificado em cada consulta.
A chave primária `(tenant_id, id)` mantém a ordenação estável. Um índice
`(tenant_id, status, id)` pode ajudar quando status é seletivo. Compare
planos com os dados reais. Use IDs positivos, únicos por tenant e
imutáveis. Inserções com ID maior podem aparecer adiante. Inserções
atrás do cursor não deslocam os registros já percorridos. A paginação
consulta a versão atual dos dados a cada request.

```sql
CREATE TABLE appointments (
  tenant_id text NOT NULL,
  id bigint NOT NULL CHECK (id > 0),
  status text NOT NULL CHECK (status IN ('scheduled', 'completed', 'cancelled')),
  PRIMARY KEY (tenant_id, id)
);
```

Configure `DATABASE_URL`, `AUTH_ISSUER`, `AUTH_AUDIENCE`,
`AUTH_JWKS_URL` e `PAGINATION_KEY`. Use um access token com tenant e
papel `professional`. `pages.query_schema()` entrega os filtros e
os parâmetros de página para `routes.document`.
