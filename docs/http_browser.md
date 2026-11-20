# TYP-015 — política HTTP para navegadores

`routes.use_cors(origins, methods, headers, credentials?)` aceita
listas explícitas. As origens são valores completos, como
`https://ui.example.org` ou `http://localhost:8081`. Use o formato
enviado pelo navegador, sem caminho ou barra final. O padrão de
`credentials` é `False`. A origem única `*` é válida para acesso
público sem credenciais. Métodos e headers precisam ser explícitos,
incluindo `Authorization` quando o navegador envia Bearer.

Um `OPTIONS` com `Access-Control-Request-Method` é avaliado antes de
admitir o request na VM. Uma combinação permitida retorna 204. A
recusa retorna 403 sem `Access-Control-Allow-Origin`. A política
preserva `Vary` da aplicação e acrescenta a origem e, no preflight,
o método e os headers solicitados. Em chamadas reais, o navegador
aplica a política ao ler a resposta. Uma requisição simples ainda
pode chegar ao servidor, portanto faça autenticação e autorização
e proteja operações mutáveis contra CSRF quando usar cookies.

`routes.use_security_headers(csp?, frame_options?, hsts_seconds?)`
habilita `X-Content-Type-Options: nosniff`. O padrão de
`X-Frame-Options` é `DENY`. Aceita `SAMEORIGIN` ou a string vazia
para omitir. CSP só é enviada quando configurada. HSTS é opcional e
só é emitido quando `net/http` tem conexão TLS comprovada. Se o
proxy termina TLS na frente de `http.serve`, configure HSTS no proxy.

`web.session_cookie(name, value, same_site?, max_age?)` gera o valor
de `Set-Cookie` com `HttpOnly`, `Secure` e `Path=/`, sem domínio.
SameSite aceita `Lax` (padrão), `Strict` ou `None`. Use `None`
apenas quando a sessão realmente precisar de cookies entre sites, com
proteção CSRF independente. Forneça o identificador opaco emitido pelo
seu backend de sessões. A vida útil padrão é a sessão do navegador
e `-1` instrui o navegador a remover o cookie.

```typhon
def session_response(id: str) -> http.Response:
    cookie: str = web.session_cookie('__Host-session', id, 'Lax', 900)
    return http.response(204, '', [['Set-Cookie', cookie]])
```

Para testar CORS, execute `typhon run examples/http_browser.ty` e, em
outro terminal, `python -m http.server 8081 --directory examples`.
Abra `http://localhost:8081/http_browser.html` e envie o JSON.
Servir a mesma página na porta 8082 produz origem rejeitada.

Referências: [Fetch Standard](https://fetch.spec.whatwg.org/#http-cors-protocol)
e [RFC 6797](https://www.rfc-editor.org/rfc/rfc6797.html).
