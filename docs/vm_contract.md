# Contrato da VM após o diagnóstico

Esta implementação corrige os nove casos P0 demonstrados no documento
`Typhon_VM_diagnostico_e_plano_de_evolucao.docx`, acrescenta proteção da ponte e
das tarefas e implementa L01/L02. Não conclui todo o plano de evolução nem torna
o backend FastAPI/Pydantic do UMarket compatível com a VM. Os próximos marcos
continuam descritos abaixo, separados do que já tem implementação e testes.

## Rastreabilidade

| Item do diagnóstico | Comportamento implementado | Teste principal |
| --- | --- | --- |
| P0 01 | Retorno obrigatório em todos os caminhos analisados; `None` implícito somente quando permitido | `TestAuditStaticRegressions` |
| P0 02 | Inicialização definida de locais, inclusive sombreamento de globais; slots não inicializados nunca representam `None` | `TestAuditStaticRegressions` |
| P0 03 | Checagem contextual de cada elemento; listas mutáveis invariantes; aliases dinâmicos conservam o contrato | `TestAuditStaticRegressions`, `TestFailedListValidationDoesNotMutateAliases` |
| P0 04 | Combinações inválidas de operadores são rejeitadas no checker e na VM | `TestAuditRuntimeRegressions` |
| P0 05 / B01 | Inteiros exatos em todo o intervalo int64; overflow e números não finitos são erros | `TestAuditPythonPrecisionAndConversions` |
| P0 06 / T02 | Validação de valores dinâmicos em atribuições, parâmetros, retornos, campos e alvos de `for` | `TestAuditPythonPrecisionAndConversions`, `TestAuditRuntimeRegressions` |
| P0 07 / B02 | Descritor privado para respostas; stdout de bibliotecas vai para stderr; protocolo 2 com sequência e limites | `TestAuditPythonOutputIsolation` |
| P0 08 / R02 | Conclusão armazenada; `join` reutilizável; espera de filhos, cancelamento e timeout | `TestAuditRuntimeRegressions`, `TestTaskCancellationAndTimeout` |
| P0 09 / R01 | Repetição negativa e tamanhos excessivos geram erro Typhon | `TestAuditRuntimeRegressions` |
| T01 | Construtores inicializam todos os campos; leituras prematuras são rejeitadas; narrowing com `is None`/`is not None` | `TestConstructorInitialization`, exemplo `user_service.ty` |
| T03 parcial | Literais preservados, uniões sem duplicatas e independentes de ordem, assinaturas comparadas, classes identificadas por definição | `TestAuditStaticRegressions` |
| B03 | Prazo por operação; fechamento concorrente ao transporte; término forçado do worker privado | `TestAuditPythonTimeoutAndRelease` |
| B07 | `typhon.python.release`, finalizers com liberação em lote, limite de handles e rollback de exportações parcialmente convertidas | `TestPythonTemporaryHandlesAreCollected` |
| B08 parcial | Tipo e traceback Python preservados em `PythonError`; handles de outra sessão são inválidos | `TestAuditPythonPrecisionAndConversions` |
| R03 | Estado mutável de uma Engine protegido por execução serializada; `join` cede execução | `TestTaskSharedListSerialized` |
| R04 parcial | Limites de instruções, profundidade, tarefas, coleções, strings e mensagens | `TestAuditRuntimeRegressions` |
| R05 | Overflow int64 explícito; comparação mista exata; política de divisão e resto | `TestControlFlowAndOperators` |
| R06 parcial | Erros com arquivo, linha e cadeia de funções Typhon | Erros verificados na suíte de regressões |
| M02 parcial | Ciclos de carregamento e execução diagnosticados; falha de módulo não é executada de novo | `TestInvalidLocalModuleDoesNotFallBackToPython` |
| M01 parcial | Erro em módulo `.ty` existente não provoca fallback Python | `TestInvalidLocalModuleDoesNotFallBackToPython` |
| L01 / L02 | `else`, `elif`, `while`, `break`, `continue`, comparações, lógica, divisão, resto e sinais | `TestControlFlowAndOperators`, `TestControlFlowErrors` |
| S01 parcial | `json.dumps` aceita floats finitos e rejeita ciclos/profundidade excessiva | `TestCyclesDoNotEscapeHost` |
| D01 / D03 parcial | Versão da CLI, este contrato, regressões, alvo de fuzzing e configuração CI Windows/Linux | `.github/workflows/ci.yml`, `FuzzParseFile` |

## Tipos e fluxo

`Any` e `PyObject` permitem chamadas dinâmicas; não representam uma inferência
silenciosa para listas heterogêneas. Ao entrar em um contrato concreto, o valor
é validado. A VM aceita promoção explícita pelo contexto de `int` para `float`,
com o arredondamento normal de float64. Não converte strings em números.
Listas existentes são invariantes: atribuir `list[int]` a `list[float]` é inválido.
Um literal novo como `values: list[float] = [1, 2]` é contextualizado e promovido.
Falhar na validação não altera parcialmente as listas de entrada.

Funções sem retorno final são válidas quando o tipo permite `None`. A análise é
conservadora: não prova que um `while True` retorna em todas as execuções.
Inicialização dentro de um loop não garante inicialização depois dele. Campos
precisam de `__init__` e de atribuições em todos os caminhos do construtor.
Inicialização indireta por outro método ainda não é inferida.

## Operadores

Precedência da menor para a maior: `or`, `and`, `not`, comparações,
`+`/`-`, `*`/`/`/`//`/`%`, sinais unários, chamadas e atributos.
`and` e `or` retornam o operando selecionado e não executam o outro lado quando
há curto-circuito; `not` retorna bool. Comparações encadeadas ainda devem ser
escritas com `and`, por exemplo `0 < x and x < 10`.

`/` retorna float. `//` arredonda para menos infinito. `%` tem o sinal do divisor.
Divisão por zero, overflow de int64 e resultado float não finito são erros.
Comparar um int64 com float64 não arredonda antes o inteiro. `+` concatena duas
strings; `*` permite string e inteiro, mas contagem negativa é um erro.

`==` em listas compara conteúdo, inclusive listas aninhadas. `is` compara a
referência da lista. Objetos e classes usam identidade. Escalares imutáveis do
mesmo tipo usam seu valor para `is`; não se promete o interning de CPython.

## Tarefas e limites

Uma Engine executa uma VM por vez. O compartilhamento de listas e objetos é
permitido, mas a execução é serializada. `join` libera o acesso à Engine enquanto
espera, permitindo executar filhos e filhos de filhos. Ao terminar o módulo
principal, o host espera pelas tarefas; falhas de filhos são propagadas.
Uma tarefa cancelada explicitamente antes da execução não roda. O cancelamento
é cooperativo e é consultado entre instruções.

`join(task)` usa 30 segundos. `join(task, seconds)` aceita um prazo positivo até
86400 segundos. Um timeout cancela a execução inteira e fecha a ponte ativa,
evitando deixar um filho bloqueado enquanto o chamador tenta retomar a Engine.
`cancel(task)` cancela aquela tarefa e o contexto herdado por seus descendentes.
O resultado de uma tarefa concluída fica disponível para joins repetidos.

| Recurso | Limite atual |
| --- | --- |
| Instruções por VM | 10 milhões |
| Profundidade de chamada e de parsing | 256 |
| Tarefas criadas por Engine | 256, incluindo concluídas |
| Elementos em `range` e crescimento por `append` | 1 milhão |
| Strings produzidas por concatenação/repetição e fonte | 16 MiB |
| Mensagem da ponte | 16 MiB |
| Profundidade de conversão e JSON | 64 |
| Handles Python vivos por sessão | 10 mil |

Esses limites são barreiras iniciais, não uma sandbox de memória ou CPU. Ainda
faltam quotas configuráveis por requisição, iteradores preguiçosos e métricas.

## Ponte Python

O protocolo 2 identifica cada requisição/resposta. `TYPHON_PYTHON_TIMEOUT` usa
durações como `250ms` ou `5s`, com padrão de 30 segundos. O prazo começa quando
a operação adquire o transporte serializado. Um timeout invalida a sessão.
O worker privado recebe até um segundo para encerrar, depois é terminado.
Erros Python recuperam o protocolo e permitem uma chamada seguinte; erros de
transporte invalidam a conexão. Não há conversão textual implícita de funções
ou objetos Typhon sem representação no protocolo.

`release` invalida aquele handle e seus aliases Go. Handles diferentes para o
mesmo objeto Python continuam independentes. Finalizers são uma recuperação
automática, sem prazo garantido; recursos que exigem liberação determinística
devem usar `release`. A coleta de handles não chama `close()` em recursos Python.
Fechar arquivos, conexões e transações continua sendo responsabilidade do programa.

No daemon compartilhado, fechar uma conexão não interrompe a função Python já
em execução. Como o interpretador serializa operações, uma chamada bloqueada
pode ocupar o daemon até terminar. O worker privado é a opção com término
forçado. Isolamento de código não confiável e callbacks Python→Typhon continuam
fora do contrato implementado.

## Próximos marcos do documento

1. Dados e recuperação: `dict[K,V]`, indexação/slices, tuplas, `try`/`except`/
   `finally`, `raise`, limpeza determinística e JSON completo.
2. Funções e módulos: defaults/kwargs Typhon, closures, aliases de import,
   `import` local unificado, `Task[T]` e assinaturas completas.
3. Integração recomendada A: API pública de embedding, host HTTP/SQL, contextos
   por requisição e uma prova de cadastro/venda/rollback/idempotência do UMarket.
   A alternativa B ainda exige callbacks, introspecção e interoperabilidade
   com classes Python antes de prometer FastAPI/Pydantic.
4. Operação: detector de race executado em ambiente com CGO, testes prolongados,
   distribuição versionada e metas de latência/memória da aplicação.

A configuração de CI adicionada executa a matriz Windows/Linux e Python
3.11/3.12/3.14, com race detector no Linux. Adicionar o workflow não equivale a
ter executado essa matriz. Os testes locais e suas limitações são informados
na entrega.
