# TYP-026 — migrations e seeds PostgreSQL

Use `DATABASE_URL` com o banco e o schema do ambiente correto.
Mantenha credenciais de aplicação e de migrations separadas.
Configure o `search_path` da conexão em um schema administrado
pela função de deploy. A tabela `typhon_schema_migrations` fica
no schema atual dessa conexão.

```sh
typhon migrate plan examples/migrations
typhon migrate up examples/migrations
typhon migrate status examples/migrations
typhon migrate down examples/migrations
```

Sem diretório explícito, os comandos leem `migrations/`.
Comece em `000001_name.up.sql` e incremente sem lacunas.
Use um `000001_name.down.sql` opcional para uma reversão escrita
e revisada. Cada arquivo contém uma instrução de nível superior.
Um bloco `DO` transacional permite várias instruções em seu corpo.
A CLI aplica cada versão com a respectiva linha de histórico na
mesma transação. Uma falha preserva o histórico das versões que
já terminaram.

`plan` lista as versões pendentes e `status` inclui as aplicadas.
O checksum SHA-256 cobre o SQL executado, com normalização de
CRLF e BOM UTF-8. Alterar ou remover a origem ou reversão de uma
versão aplicada bloqueia o comando. Faça mudanças por uma versão
nova. O histórico verifica a origem das migrations. Alterações manuais
no catálogo do PostgreSQL exigem auditoria separada.

Uma conexão segura um advisory lock por banco durante a execução.
Se outra execução já estiver migrando, a nova chamada espera até
30 segundos. `up` é idempotente. `down` reverte apenas a última
versão aplicada e exige o SQL de reversão. A CLI imprime versão,
nome, estado e checksum sem imprimir conexão ou SQL.

Para criar ou remover índices com `CONCURRENTLY`, coloque
`-- typhon:no-transaction` na primeira linha. A CLI grava
`applying` ou `reverting` antes de enviar a instrução. Uma
interrupção deixa esse estado visível e impede novas operações.
Inspecione o catálogo e os índices inválidos antes de reconciliar
o histórico com o DBA. Dados removidos e outros DDLs irreversíveis
podem exigir recuperação por backup.

Use seeds separadamente, só em desenvolvimento ou teste.
`typhon migrate seed` lê `seeds/` por padrão e exige
`TYPHON_ENV=development` ou `TYPHON_ENV=test`. As seeds rodam
em uma transação e são executadas novamente a cada invocação.
Escreva-as de forma idempotente quando repetição for desejada.

```sh
TYPHON_ENV=test typhon migrate seed examples/seeds
```

O job `migrations` do CI aplica os exemplos num PostgreSQL
descartável, repete `up`, volta uma versão e aplica a seed.
No deploy, execute o comando uma vez como etapa própria e
só inicie as novas réplicas após sucesso. A inicialização do
servidor Typhon não aplica migrations nem seeds.

[Advisory locks no PostgreSQL](https://www.postgresql.org/docs/16/explicit-locking.html#ADVISORY-LOCKS)
