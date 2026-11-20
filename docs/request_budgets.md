# TYP-007 — orçamentos por requisição

Antes, os tetos de 8 MiB de HTTP, 100 mil linhas SQL e 10 milhões
de instruções eram verificados separadamente. Uma requisição podia
acumular alocações entre chamadas e repartir trabalho entre tarefas.
O runtime agora usa contadores cumulativos por requisição e por
execução de módulo fora do HTTP. Tarefas descendentes e importações
locais executadas no mesmo escopo compartilham os contadores.

| Ambiente | Padrão | Faixa |
| --- | --- | --- |
| `TYPHON_LIMIT_ENGINE_BYTES` | 256 MiB | 1 KiB a 512 MiB |
| `TYPHON_LIMIT_REQUEST_BYTES` | 64 MiB | 1 KiB a 256 MiB |
| `TYPHON_LIMIT_INSTRUCTIONS` | 10 milhões | 1 a 50 milhões |
| `TYPHON_LIMIT_SQL_ROWS` | 100 mil | 1 a 100 mil |
| `TYPHON_LIMIT_HTTP_QUEUE` | 256 vagas máximas | 0 a 256 |

Informe bytes, instruções, linhas e vagas como inteiros decimais.
A Engine lê a configuração ao iniciar e rejeita valores fora da
faixa. `http.serve()` limita sua fila à configuração. Com a chamada
padrão seleciona até 15 vagas, conforme o teto configurado.

O peso lógico de um slot Typhon é 160 bytes. Os contadores incluem
corpos HTTP, JSON, strings produzidas, listas e dicionários, campos
de objetos, resultados nativos e materialização SQL. SQL contabiliza
bytes de colunas além de linhas. O pgx limita cada mensagem a até
64 MiB, ou ao teto da Engine se menor. O limite de transporte HTTP de
8 MiB e os tetos próprios do codec JSON continuam valendo.
Resultados Redis são contabilizados na conversão para valores Typhon.

Esgotamento gera `*typhon.QuotaError` com recurso e limite, acessível
no host via `errors.As`. No HTTP, corpo grande recebe 413 e
esgotamento durante execução ou materialização recebe 422. O runtime
encerra o bytecode e roda o cleanup nativo do escopo, incluindo rollback.
`http.server_stats()` contabiliza esses casos em `quota_rejected`.
Os contadores são reiniciados para o próximo request.

O orçamento mede trabalho e alocações selecionadas, com pesos
conservadores e sem crédito por liberação. Os limites do container
continuam responsáveis por RSS e CPU reais, incluindo memória do
host, runtimes externos e buffers temporários dos drivers.
Use streaming para datasets maiores quando o driver oferecer essa API.

Para executar [request_budget.ty](../examples/request_budget.ty),
configure `TYPHON_LIMIT_REQUEST_BYTES=4096` e
`TYPHON_LIVENESS_PATH=/_live`. A rota `/large` recebe 422 e
`/` continua disponível. A rota `/_live` responde 204 em Go antes da
fila e da VM, inclusive quando todos os workers estão ocupados. Escolha
um caminho reservado que o aplicativo possa dedicar ao probe.
