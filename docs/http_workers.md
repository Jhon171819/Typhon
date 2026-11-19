# TYP-005 — workers HTTP isolados

## Decisão

A Engine mantém globais mutáveis e protege a execução Typhon com
`execMu`. O modelo anterior colocava todos os callbacks HTTP nessa
mesma trava, inclusive durante I/O. Tirar a trava exigiria sincronizar
listas, dicionários, classes e inicialização de módulos compartilhados.
O pool usa uma Engine por worker e agenda uma requisição por worker.
Cada Engine mantém sua própria memória Typhon e registra seus
próprios recursos.

`http.serve(address, handler)` usa um worker e aceita até 15 requisições
na fila, preservando até 16 admissões simultâneas. Para habilitar
concorrência, passe `http.serve(address, handler, workers, queue)`.
O servidor aceita de 1 a 16 workers e de 0 a 256 vagas de fila. Uma
fila cheia recebe 503 imediatamente. Cancelamento retira uma
requisição da fila, que consome o [prazo HTTP](request_context.md)
configurável no quinto argumento. Encerrar o servidor fecha os
workers auxiliares.

O worker primário usa a Engine atual. Os auxiliares carregam e
executam uma vez o módulo que define o handler, com `__name__`
definido como nome do módulo. Coloque `serve()` sob
`if __name__ == '__main__':` e mantenha a inicialização por módulo
segura para repetição. Variáveis globais de um worker persistem
nesse worker. Use PostgreSQL ou Redis para estado compartilhado.

Inicialize clientes PostgreSQL, Redis ou HTTP no módulo para
reutilizá-los nas requisições daquele worker. Com um pool PostgreSQL
de 16 conexões por worker, quatro workers admitem até 64 conexões
potenciais desse pool. Dimensione o número de workers para o limite
do banco. O teste de integração confere reuso de três backends por
três workers. A memória de respostas e globals também se multiplica
com o número de workers.

## Operação e medição

`http.server_stats()` fornece métricas compartilhadas da instância.
`admitted` conta admissões, `rejected` conta saturação ou cancelamentos
antes de executar, e `completed` conta saídas de handlers. `queued`
é a fila atual, `queue_peak` o pico e `queue_wait_ns` a soma dos
tempos de espera. O backend aplica controle de acessos quando expõe
essas métricas em rede.

Execute [http_workers.ty](../examples/http_workers.ty) para ver o
contador local por worker e as métricas da instância. O benchmark
reproduz a comparação de 1 e 4 workers com um upstream HTTP local.

```powershell
go test ./internal/typhon -run '^$' -bench '^BenchmarkNativeHTTPWorkerThroughput$' -benchtime 100x -cpu 8
```

O benchmark inclui p50, p95, p99, throughput, bytes e alocações por
operação. Esses números caracterizam a configuração local e a carga
sintética, com I/O no upstream. O teste com barreira confirma
progresso simultâneo independentemente da velocidade da máquina.
