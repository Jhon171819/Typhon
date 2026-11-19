# TYP-011 — middleware e contexto do request

`Router.use(middleware)` registra um middleware global.
`Router.use_on(method, pattern, middleware)` adiciona um middleware
a uma rota já registrada. O contrato é:

```typhon
def process(req: http.Request, next: web.Next) -> http.Response:
    req.set_state('action', 'load')
    return next.call()
```

Os globais executam primeiro, em ordem de registro. Em seguida
executam os da rota, na mesma ordem. O código depois de
`next.call()` roda na volta, em ordem inversa. Retornar uma
resposta antecipadamente encerra a cadeia. Use `except` para erros
tipados de etapas posteriores e `finally` para limpeza. O prazo e
o cancelamento são os mesmos do handler final.

Cada objeto `Next` aceita uma chamada durante o middleware que o
recebeu. Uma chamada repetida ou depois do retorno gera
`ValidationError`. O router aceita 16 etapas globais e 16 por rota.
Respostas automáticas do roteador, como um 404 sem rota, saem antes
da cadeia de uma rota.

O request oferece `state(name) -> str | None` e
`set_state(name, value)`, com até 64 chaves, nome de até 64 bytes
e valor UTF-8 de até 1024 bytes. O estado pertence ao request e usa
strings copiáveis. `request_id`, `identity` e `tenant` são campos
somente de leitura. ID é gerado no servidor. Identidade e tenant
ficam em `None` até que uma integração nativa confiável forneça
metadados verificados. Escrever `identity` no estado ou enviá-lo
em um header do cliente não altera esses campos.

`use_request_id()` é um middleware nativo que inclui o ID em
`X-Request-ID` na resposta. `use_logging()` registra ID, método,
padrão de rota, status ou falha e duração. O log usa o padrão da
rota e omite o corpo, a query e as credenciais.

O [exemplo HTTP](../examples/http_middleware.ty) usa dois workers,
request-id, logging e mapeamento de `ValidationError` para 400.
A [fronteira de erros](http_problems.md) oferece um contrato
comum para as exceções que escapam do pipeline.
