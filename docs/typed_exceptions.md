# TYP-001 — exceções tipadas na VM

## Contrato

Antes de TYP-001, o parser e o checker desconheciam exceções. Uma falha
interrompia a execução de bytecode e passava pelo retorno Go, com arquivo,
linha e função acrescentados em cada chamada Typhon. HTTP atendia requisições
posteriores, mas um callback não podia escolher uma resposta para uma falha do
driver. Algumas causas Go incluíam URLs, DSNs ou mensagens enviadas pelo banco.

Cada bloco deve ter pelo menos uma instrução.
`try` exige `except Tipo` ou `finally`. `else` exige pelo menos um `except`.
Os handlers testam, em ordem, o tipo da exceção e a relação de subtipo. `Error`
é a raiz de `ValidationError`, `TimeoutError`, `ConnectionError` e `DatabaseError`.
Cada subtipo é independente. O checker rejeita handlers inalcançáveis por
duplicação ou por um handler anterior mais abrangente. `as nome` vincula um
valor somente para leitura, visível dentro daquele `except`.

`else` roda quando o corpo termina normalmente. Uma exceção no `else`, em
um `except` ou no `finally` segue para a próxima região externa. `finally`
roda uma vez quando a região termina normalmente ou é abandonada por exceção,
`return`, `break` ou `continue`. Um novo desvio que sai do `finally`
substitui um retorno ou desvio pendente. Uma falha de limpeza durante a
propagação de outro erro é registrada e a causa original continua a
propagar. Um desvio interno a um loop do `finally` deixa o desvio pendente
intacto. Um `return` explícito no `finally` pode encerrar a propagação de
um erro comum. O cancelamento habilita a limpeza limitada e continua a propagar
fora dos `except`. Consulte os prazos e o fechamento nativo no
[contrato de limpeza](resource_cleanup.md).

`raise ValidationError('mensagem')` cria uma falha de aplicação. Os
construtores aceitam uma `str` posicional. Os valores expõem `kind` e `message`,
ambos `str`, e podem passar por `Any` e por contratos que aceitem seu tipo ou
`Error`. `raise valor` valida o tipo em tempo de execução se ele for dinâmico.
`offset` contém a posição em bytes de um erro de entrada JSON ou `None`.
`raise` sem argumento, dentro de um `except`, propaga o erro capturado com o
traceback original. `raise valor` inicia um traceback naquele ponto e preserva
a causa nativa do valor, quando houver.

O checker assume que um erro pode ocorrer desde a primeira instrução do
`try`. Inicialize antes do `try` os valores usados na limpeza e nos handlers.
Erros de compilação, falhas legadas como divisão por zero ou índice inválido
e erros da ponte Python continuam fora da hierarquia. Uma falha legada de
execução ainda percorre os blocos `finally` quando o contexto está ativo.

## Drivers e diagnóstico

| Tipo | Origem |
| --- | --- |
| `ValidationError` | Validação de argumentos, configuração e uso dos drivers, incluindo chamadas dinâmicas e recursos já fechados |
| `TimeoutError` | Prazo de uma operação HTTP, PostgreSQL ou Redis com o contexto da VM ainda ativo |
| `ConnectionError` | Falhas de conexão ou erros de rede identificados pelos adaptadores |
| `DatabaseError` | Falhas de operações e respostas de erro de PostgreSQL e Redis |
| `Error` | Tipo base e construtor de falhas genéricas de aplicação |

Códigos HTTP 4xx e 5xx continuam sendo respostas normais. Falhas de driver
preservam os frames Typhon e uma causa nativa com origem e diagnóstico
normalizados. PostgreSQL também preserva o SQLSTATE válido quando disponível.
URLs, DSNs, endereços, comandos, credenciais e mensagens brutas dos drivers
ficam fora dessa cadeia. Mensagens criadas pela aplicação são exibidas no
traceback, portanto use texto apropriado para logs em `raise`.

Em Go, `errors.As` encontra `*typhon.Exception` e, quando aplicável,
`*typhon.NativeCause`. Prazos nativos que encapsulam
`context.DeadlineExceeded` mantêm esse marcador em `errors.Is`. Uma exceção
tipada que escapa do módulo principal é devolvida ao host e cancela tarefas
pendentes, com a Engine disponível para outros módulos. Um `join` entrega o
resultado do filho ao chamador, inclusive quando falha. Falhas de filhos cujo
resultado ficou sem observação são reportadas ao fim da execução principal.

## Exemplos

[O handler HTTP](../examples/typed_exceptions.ty) mapeia validação para 400,
falha de conexão para 502 e timeout para 504. Rode
`go run ./cmd/typhon run examples/typed_exceptions.ty`. O endpoint
`/health` responde na porta 8080. A rota `/?name=Ada` usa o serviço de
perfil local na porta 8081, caso esteja disponível.

Este exemplo usa o mesmo pool depois de um erro de consulta. Uma transação
PostgreSQL abortada exige rollback antes de reutilizar a conexão.

```typhon
import typhon.postgres as pg

db: pg.Connection = pg.connect(pg.env('DATABASE_URL'))
try:
    try:
        db.query('SELECT missing_column')
    except DatabaseError as err:
        print(err.kind)
    else:
        print('consulta aceita')
    print(db.query('SELECT 1 AS id').get(0, 'id'))
finally:
    db.close()
```

## Implementação

Uma região de bytecode registra o fim do corpo protegido, os destinos dos
handlers e o destino do `finally`. A pilha de regiões de cada frame guarda a
altura da pilha de operandos e o desvio pendente. A VM reutiliza o retorno Go
entre frames, restaurando operandos antes de entrar em um handler ou `finally`.
Isso permite usar a mesma rota para exceções, retornos e saídas de loop e evita
duplicar bytecode de `finally` em cada saída. A alternativa de retornar códigos
de erro de cada driver exigiria alterar as assinaturas e espalhar verificações
pelos chamadores.
