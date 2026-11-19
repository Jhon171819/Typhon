# TYP-002 — limpeza determinística de recursos

## Escopos e ordem

O fechamento automático anterior usava um mapa sem ordenação e descartava
falhas secundárias. A VM também pulava o `finally` ao receber cancelamento.
A TYP-002 reaproveita as regiões de `finally` da TYP-001 e o registro nativo
de escopo. Um `defer` ou gerenciador de contexto exigiria outra forma de
registrar e executar limpeza.

| Escopo | Encerramento |
| --- | --- |
| Engine | `Engine.Close()` cancela e aguarda as execuções e tarefas ativas, depois fecha os recursos da Engine. A CLI faz isso ao sair, inclusive em erro. |
| Requisição HTTP | Na saída do handler, o runtime cancela e aguarda as tarefas da requisição. O fechamento termina antes de enviar a resposta. |
| Pool PostgreSQL | O fechamento encerra transações abertas antes de fechar conexões. Um pool criado na Engine pode servir a vários requests, cada um com suas próprias transações. |
| Cursor e corpo HTTP | A consulta PostgreSQL materializa `Rows` e fecha o cursor ao sair da operação. O cliente HTTP lê e fecha o corpo antes de devolver a resposta. |

O runtime fecha recursos ativos na ordem inversa da aquisição dentro de
cada escopo. `close()` e `rollback()` explícitos retiram o registro e são
idempotentes. O pool também registra suas transações para atender a
`db.close()` explícito. Uma transação em mais de um registro ainda termina
uma única vez. Somente `commit()` confirma mudanças. Quando o PostgreSQL
falha ao executar rollback, o pgx descarta a conexão e devolve a vaga ao pool.

Objetos nativos e seus métodos vinculados permanecem associados à Engine.
Clientes, transações e objetos `Request` criados em um request também exigem
o mesmo request no uso posterior. Guarde na Engine o pool ou cliente que
precisa ser compartilhado. Resultados já materializados, como `Rows` e
`Response`, podem ser usados por outros requests da mesma Engine.
Tarefas de request devem ser aguardadas com `join` quando seu resultado
for necessário. Os slots da cota da Engine usados por tarefas de request
voltam a ficar disponíveis ao terminar o request.

## Finally, cancelamento e diagnóstico

`finally` continua rodando em saída normal, retorno, desvio de loop e
exceção. Quando o contexto cancela ou expira, a VM pula os `except` e
percorre os `finally` já ativos de dentro para fora. A VM usa um prazo
compartilhado de 2 segundos e até 10 mil instruções para esse código.
Nesse modo, as operações nativas de liberação são `close()` e
`rollback()`. `Request.remaining()` e `Request.cancelled()` podem
inspecionar o contexto original. Novos I/O de driver, imports, chamadas à ponte Python
e novas esperas ou tarefas são rejeitados. Um desvio em `finally` mantém
o cancelamento pendente. Limites rígidos da VM encerram bytecode e o
runtime fecha os registros do escopo ao sair.

Uma falha em `finally` com outro erro pendente é registrada com traceback e
o erro original continua a propagação. Falhas do fechamento automático
incluem o recurso e a operação. A CLI retorna a causa primária seguida
dos erros de fechamento, quando ambos existem. O handler registra a
cadeia. Uma falha de limpeza sem outra causa gera 500. As respostas
de cancelamento seguem o [contrato HTTP](request_context.md), e os
detalhes de driver continuam sanitizados.

O prazo de 2 segundos para rollback é compartilhado dentro de cada
drenagem de escopo. A destruição das conexões pelo `pgxpool` é síncrona
e usa o prazo interno do driver, de até 15 segundos por conexão. O
servidor HTTP usa até 5 segundos para shutdown gracioso antes de fechar
sockets ativos. Esses prazos se aplicam às respectivas fases. A VM
depende de operações do host retornarem, inclusive escrita em stdout.

## Uso

Execute [postgres_cleanup.ty](../examples/postgres_cleanup.ty) com um
`DATABASE_URL` de teste e uma tabela `profiles (name text)`. A primeira
chamada retorna antes de confirmar a inserção. A segunda confirma
explicitamente. O `rollback()` do `finally` também é seguro após o commit.

Faça chamadas HTTP antes de abrir a transação ou depois de encerrá-la.
Deixar uma transação aberta enquanto aguarda um serviço externo mantém
uma conexão do pool reservada e pode prolongar bloqueios no banco.
