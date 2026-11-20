# TYP-014 — autorização HTTP por papel e tenant

`auth.policy(roles, required_scopes?)` cria uma política imutável. O primeiro
argumento associa cada papel a ações permitidas. O segundo pode exigir um
scope para uma ação, além do papel. Depois de `routes.add(...)` e
`routes.require_auth(...)`, associe a política com
`routes.authorize(method, pattern, policy, action)`. Uma ação ausente da
política é erro de configuração. A fronteira exige tenant e papel e, quando
configurado para a ação, o scope. Autorize cada endpoint sensível
explicitamente, incluindo métodos adicionais para o mesmo caminho.

Configure o emissor para incluir `tenant` (tenant ativo), `roles` (array)
e `scope` (string com scopes separados por espaço) nas claims assinadas.
O emissor deve verificar a associação ao tenant antes de emitir o token.
Headers, payload e parâmetros de caminho enviados pelo cliente não
atribuirão tenant, papéis ou scopes à requisição. Falha de credencial
retorna 401. A política responde 403 quando o principal verificado carece
de tenant, papel ou scope. Os logs da política registram ID da requisição,
padrão da rota, ação e decisão.

No [exemplo](../examples/http_tenant_authorization.ty), `admin` pode
ler, criar e excluir, com scope adicional para exclusão. `professional`
pode ler e criar. `user` pode ler. Todos operam somente sobre seus
próprios registros no tenant ativo. Execute a migração em um banco de
teste com credenciais separadas das de produção.

```sql
CREATE TABLE records (
  tenant_id text NOT NULL,
  id text NOT NULL,
  owner_id text NOT NULL,
  content text NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
```

Configure `DATABASE_URL`, `AUTH_ISSUER`, `AUTH_AUDIENCE` e
`AUTH_JWKS_URL`. Execute `typhon run examples/http_tenant_authorization.ty`
e use os JWTs do seu provedor. Consultas e mutações incluem tenant e dono
verificados na cláusula `WHERE` da própria operação. O caminho fornece só
o identificador. Tanto um registro ausente quanto um registro de outro
tenant ou dono retornam 404. As inserções derivam tenant e dono do
principal verificado.

A VM protege a entrada da rota. Mantenha as restrições de linha no
repositório de dados e, quando a aplicação usar PostgreSQL RLS, faça
o papel de conexão passar pelas políticas. Prefira uma configuração de
tenant limitada à transação ao usar RLS com pools.
