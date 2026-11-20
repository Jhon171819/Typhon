# TYP-012 — erros HTTP estruturados

`Router.use_errors()` instala uma fronteira de erros em torno das
rotas e aplica o mesmo formato aos erros do servidor, incluindo
404, 405, fila cheia, prazo e quota. A configuração é opt-in.
Handlers HTTP simples conservam suas respostas anteriores.

O corpo `application/problem+json` tem quatro campos estáveis:

```json
{
  "code": "validation_error",
  "message": "The request violates its contract.",
  "details": [],
  "request_id": "ID_GERADO_PELA_ENGINE"
}
```

`X-Request-ID` repete o ID do corpo. Mensagem e código vêm de uma
tabela fixa. Os erros de schema incluem os diagnósticos gerados pelo
validador, com path, regra, código e mensagem. Os valores rejeitados,
DSN, SQL, credenciais e stack ficam fora do envelope. O log da
fronteira registra ID, padrão de rota, classe, driver e SQLSTATE
quando disponível.

| Origem | Status e código |
| --- | --- |
| JSON malformado | 400 `bad_request` |
| Autenticação ou autorização rejeitada pela aplicação | 401 `unauthorized`, 403 `forbidden` |
| Recurso ausente ou método ausente | 404 `not_found`, 405 `method_not_allowed` |
| Rejeição de conflito ou SQLSTATE PostgreSQL 23505 | 409 `conflict` |
| Schema, validação ou quota de execução | 422 `validation_error` |
| Limite de consumo da aplicação | 429 `too_many_requests` |
| Dependência indisponível, fila cheia ou shutdown | 503 `service_unavailable` |
| Timeout de driver ou prazo HTTP | 504 `timeout` |
| Falha interna ou falha SQL sem mapeamento público | 500 `internal_error` |

O transporte conserva os status 408, 413 e 499 quando aplicáveis.
`Allow` continua disponível em 405. Para distinguir rejeições de
domínio e erros internos, use `raise web.reject(status)` ou retorne
`web.problem(req, status)`. Ambos usam status admitidos da tabela.
`web.validate(req, schema, value)` retorna `None` quando o payload
é aceito e uma resposta 422 quando há falhas de schema.

Antes da fronteira, um middleware Typhon pode capturar uma exceção,
aplicar uma política de domínio e devolver sua própria resposta.
Cancelamento e limites obrigatórios continuam propagando ao
runtime. O servidor só envia o status depois de materializar a
resposta. Depois de gravados os headers, o status já enviado
permanece. Uma futura API de streaming terá de sinalizar falhas
posteriores pelo protocolo do stream.

O [exemplo HTTP](../examples/http_problem.ty) diferencia JSON
malformado, dados inválidos, sucesso e indisponibilidade.
