# TYP-020 — Server-Sent Events

Registre um GET no router e chame
`routes.enable_sse(pattern, max_seconds?, heartbeat_seconds?)`.
O handler devolve `http.sse(producer, state)`. A assinatura do
produtor é `(str, Any) -> http.Event | None`. O primeiro argumento
é o último ID visto pela conexão. O segundo é o estado passado
pelo handler. `None` indica que ainda não há evento. A VM volta
a chamar o produtor periodicamente, com intervalo nominal de 250 ms.

`http.event(id, event_type, data)` codifica um evento UTF-8 em até
16 KiB de dados. Use ID vazio para mensagens sem ID. Deixe o tipo
vazio para o evento padrão `message`. Dados com mais de uma linha
são codificados como campos `data:` distintos. Use dados em JSON
quando o cliente precisar de uma estrutura definida.

Na reconexão, a VM passa `Last-Event-ID` ao produtor. Escolha IDs
estáveis e consulte eventos depois do último ID. A VM elimina a
repetição do último ID da conexão. O produtor continua responsável
pelo histórico e por suas regras de retenção. Redis Streams ou
um banco compartilhado podem manter esse histórico entre réplicas.

Cada conexão envia um comentário inicial e heartbeats com flush.
O prazo padrão de negociação e de cada chamada ao produtor é o
prazo HTTP configurado. A duração da conexão é independente, de
1 a 1.800 segundos, com padrão de 300. Heartbeats aceitam de 1 a
60 segundos, com padrão de 15. Há até 128 conexões de eventos
simultâneas por servidor. SSE tem admissão própria e só usa
o lock da VM enquanto executa o produtor. Faça callbacks breves.
As escritas são síncronas, com teto de dois segundos por
operação, e o runtime não acumula uma fila de eventos por cliente.
`http.server_stats()` expõe `event_connections`.

Use `routes.require_auth` e `routes.authorize` antes do handler de
eventos quando precisar de controle de acesso. A conexão encerra
na expiração do JWT ou no limite de duração, o que ocorrer primeiro.
Na reconexão, o cliente deve apresentar credenciais válidas novamente.
Clientes que precisam de um cabeçalho Bearer podem usar fetch com
leitura incremental. O `EventSource` nativo do navegador não aceita
um cabeçalho Authorization personalizado.

O desligamento cancela conexões ativas e executa a limpeza de seus
recursos. O teste local com 100 conexões, depois de GC, mediu cerca
de 3,6 MB adicionais somando cliente e servidor no mesmo processo.
O teste também enviou uma requisição HTTP comum enquanto todas as
vagas SSE estavam ocupadas. WebSocket é uma extensão separada do
lifecycle SSE entregue aqui.

Execute [`http_events.ty`](../examples/http_events.ty) e reconecte
informando o último evento.

```sh
curl -N http://127.0.0.1:8080/v1/events
curl -N -H 'Last-Event-ID: 1' http://127.0.0.1:8080/v1/events
```

[Especificação Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
