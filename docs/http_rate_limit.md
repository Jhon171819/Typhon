# TYP-016 — limite distribuído de requisições

Crie `web.rate_limiter(cache, namespace, limit, window_seconds, fail_open?, plan_limits?)`
e associe com `routes.rate_limit(method, pattern, limiter)` depois de
`routes.require_auth(...)`. Use o mesmo Redis, namespace e configuração
para todas as réplicas do serviço. Use namespaces distintos para aplicações
ou ambientes. `plan_limits` mapeia scopes assinados a quotas alternativas.
Se mais de um scope corresponder, vale a menor quota entre os planos
correspondentes. Na ausência de plano, vale o limite padrão.

O contador usa tenant e assunto do principal autenticado, método, padrão
da rota e namespace. Os identificadores são digeridos antes de compor
as chaves Redis. A política ignora `X-Forwarded-For`, headers de tenant,
IP e parâmetros de caminho. Para proteger endpoints públicos, aplique
limites no proxy que autentica o encaminhamento.

O script Lua mantém uma janela fixa a partir da primeira chamada que
consome a quota. Incremento, inspeção da quota e expiração acontecem
juntos no Redis. Chamadas recusadas não estendem a janela. O índice mantém
até 4.096 identidades ativas por combinação de namespace, método e rota.
Quando lotado, uma identidade nova recebe 429 até liberar uma vaga.
Chamadas podem se concentrar ao redor da virada de janela. Use uma
janela mais curta para reduzir essa concentração.

Respostas processadas pela política incluem `X-RateLimit-Limit`,
`X-RateLimit-Remaining` e `X-RateLimit-Reset`, em segundos relativos
para o reset. Uma recusa 429 inclui também `Retry-After`, em segundos.
`http.server_stats()` registra `rate_allowed`, `rate_denied` e
`rate_errors` por processo. Configure o Redis com capacidade para o
total de rotas e sem evicção antecipada dessas chaves se os limites
forem obrigatórios.

O Redis recebe até 150 ms, sujeito ao prazo principal do request.
Uma falha causa 503 por padrão. Se `fail_open=True`, a rota prossegue
sem quota naquela tentativa e registra a falha. A configuração do
cliente no [exemplo Typhon](../examples/http_rate_limit.ty) adia a
conexão para que a política também se aplique quando o Redis estiver
indisponível na inicialização. Mantenha fail-closed em rotas onde a
quota é necessária para proteger custo ou integridade.
