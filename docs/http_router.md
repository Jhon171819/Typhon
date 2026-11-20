# TYP-010 — roteamento HTTP

`typhon.web` fornece um router opcional sobre os tipos
`http.Request` e `http.Response`. O servidor continua aceitando
funções `(Request) -> Response` e também pode receber um
`web.Router`.

```typhon
import typhon.http as http
import typhon.web as web

def show(req: http.Request) -> http.Response:
    return http.json(200, {'id': req.param('id')})

routes: web.Router = web.router()
routes.add('GET', '/v1/users/{id}', show)

if __name__ == '__main__':
    http.serve('127.0.0.1:8080', routes)
```

Registre as rotas no início do módulo. `add(method, pattern, handler)`
aceita métodos em maiúsculas e caminhos absolutos. Cada `{param}`
ocupa um segmento completo. O router permite 256 rotas, caminhos
de até 4096 bytes e até 32 segmentos. Padrões com mesma forma
estrutural e método conflitam, mesmo se o nome de parâmetro mudar.
A tabela fica imutável ao começar a servir. Para usar a tabela em
uma função existente, chame `routes.dispatch(req)`.
[Middlewares](http_middleware.md) podem envolver o handler globalmente
ou por rota registrada.

O trie prioriza o caminho literal que tiver correspondência completa.
Se o ramo literal acabar sem rota, tenta o parâmetro. O router
escolhe o caminho antes do método, portanto uma rota estática pode
retornar 405 mesmo se houver um padrão dinâmico mais genérico com
outro método.

O roteamento decodifica cada segmento uma vez. Aceita UTF-8 codificado,
como `caf%C3%A9`, e rejeita barras codificadas, barras invertidas,
segmentos vazios, ponto, ponto duplo, controles e escapes ambíguos
que deixem `%` após a decodificação. O erro 400 omite o caminho recebido.

404 indica ausência de rota. 405 devolve `Allow` para o caminho
selecionado. GET também fornece HEAD por padrão, com o comprimento
da representação e sem corpo. OPTIONS é automático e retorna 204
com `Allow`. É possível registrar handlers HEAD e OPTIONS explícitos.

`req.param(name)` e `req.query(name)` devolvem string vazia se o
campo estiver ausente. `req.query_all(name)` devolve a lista em ordem.
`req.header(name)`, `http.response()` e `http.json()` continuam
disponíveis. Use `/v1` para versionar rotas por recurso. O exemplo
mostra `limit` e `offset` como parâmetros de consulta. A aplicação
faz a validação dos parâmetros e escolhe offset ou cursor.

O router deve estar em uma global do módulo para iniciar workers
isolados. Cada worker carrega o mesmo módulo e compila sua própria
tabela uma vez. O [exemplo CRUD](../examples/http_router.ty) usa um
worker para manter o armazenamento local em memória. Use os drivers
compartilhados de banco ou cache para estado comum entre workers.
