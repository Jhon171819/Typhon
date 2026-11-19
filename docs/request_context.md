# TYP-006 — contexto e prazos HTTP

O servidor combina seu contexto com o do cliente e define um prazo
absoluto na admissão. Fila, leitura do corpo e callback compartilham
esse prazo. Cada operação HTTP, PostgreSQL ou Redis recebe o menor
prazo entre o da requisição e o timeout configurado no cliente do
driver. Chamadas sucessivas consomem o mesmo prazo da requisição.

`http.serve(address, handler, workers, queue, request_timeout)`
aceita por último um prazo positivo em segundos de até 300. O
padrão é dez segundos e as assinaturas posicionais anteriores
continuam disponíveis. `Request.remaining()` devolve os segundos
restantes e `Request.cancelled()` indica cancelamento ou expiração.
Ambos consultam o contexto original, inclusive no `finally` de
cancelamento.

Cancelar o contexto interrompe o bytecode e a aquisição de novos
recursos. Tarefas descendentes recebem o mesmo contexto. Um timeout
em `join` dentro do handler cancela sua requisição e preserva o
worker para o próximo cliente. Fora do servidor, `join` mantém o
contrato de cancelamento da execução inteira.

O rollback automático usa o prazo de limpeza separado. Redis
interrompe I/O bloqueado em uma conexão cuja operação foi cancelada
e descarta o transporte quando a resposta pode ficar pendente.
HTTP usa o contexto nos requests de saída. PostgreSQL usa o contexto
na execução e o runtime também o verifica ao materializar resultados.

| Resultado | Resposta quando o socket ainda pode receber dados |
| --- | --- |
| Encerramento do servidor ou fila cheia | 503 |
| Prazo da requisição esgotado, inclusive em fila | 504 |
| Prazo local do driver propagado sem captura | 504 |
| Cliente cancelou | 499 para diagnóstico, com entrega dependente do socket |
| Prazo de leitura do socket expirou antes do prazo da requisição | 408 |
| Outro erro de execução | 500 |

Prazos exclusivos de uma operação continuam capturáveis como
`TimeoutError`. Cancelamento da requisição segue pelos `finally`
limitados e fica fora de `except`. O servidor materializa a resposta
antes de escrever os headers. Depois de iniciada a escrita, o status
já enviado permanece. Na CLI, SIGINT e SIGTERM acionam o cancelamento.

Inicialize módulos e pools compartilhados antes de iniciar o servidor.
Uma importação Typhon executada pela primeira vez em um callback
herda seu contexto. O runtime memoriza uma inicialização interrompida
para evitar repetir seus efeitos parciais.

O [exemplo da CLI](../examples/request_deadline.ty) usa prazo de
1,5 segundo e dois workers. A rota `/remaining` mostra o orçamento
que chegou ao handler.
