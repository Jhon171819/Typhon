# TYP-013 — autenticação de rotas HTTP

Crie um `auth.Authenticator` no carregamento do módulo e associe-o a cada
rota privada com `routes.require_auth(method, pattern, verifier)` depois de
registrá-la. A verificação precede os middlewares e o handler. Uma rota pública
continua disponível sem credenciais. O exemplo executável está em
[`examples/http_auth.ty`](../examples/http_auth.ty). Substitua o emissor,
a audiência e a URL de chaves do exemplo pela configuração do seu provedor.

O provedor precisa emitir access tokens JWT compactos, com `typ` igual a
`at+jwt` ou `application/at+jwt`, assinatura RS256 e `kid` presente no
JWKS configurado. A VM verifica assinatura, emissor, audiência, assunto e
expiração obrigatória. Valida `nbf` e `iat` quando presentes, sem tolerância
de relógio. Configure uma audiência própria para a API e sincronize seu
relógio. Identidade e tenant opcionais de rotas públicas permanecem ausentes,
salvo quando fornecidos pelo host Go. Em rotas protegidas, `req.identity`
contém o `sub` verificado e `req.tenant` vem da claim assinada `tenant`,
quando presente. Os campos são somente leitura. A aplicação continua
responsável por autorização de negócio.

O cliente usa `Authorization: Bearer <access_token>` em uma conexão HTTPS.
Credenciais ausentes ou inválidas recebem 401 com `WWW-Authenticate: Bearer`.
Uma falha de JWKS sem chave válida para o token recebe 503. Com
`routes.use_errors()`, ambas usam o envelope de erros HTTP. Mantenha
o cabeçalho Authorization fora de logs de aplicação, proxy e observabilidade.

Cada Engine mantém seu próprio cache. O TTL padrão é 60 segundos e aceita
de 1 a 300 segundos. Um `kid` desconhecido pode buscar as chaves novamente,
respeitando o intervalo mínimo de um segundo. Uma chave expirada sai de uso
até uma atualização bem-sucedida. A busca usa HTTPS com validação de
certificado, sem redirecionamento, prazo máximo de dois segundos e limite de
64 KiB e 32 chaves. Use um JWKS de configuração confiável e publique chaves
novas antes de emitir tokens que as referenciem.

Tokens stateless permanecem válidos até a expiração ou até a retirada da
chave e atualização do cache. Use expirações curtas e faça emissão e refresh
no provedor. Se o aplicativo web precisar encerrar sessões imediatamente,
um backend de sessão deve revogar o identificador no servidor. Para cookies
de sessão, use `HttpOnly`, `Secure`, `SameSite` e proteção CSRF nas
operações mutáveis. Mantenha autenticação de sessão e login local no provedor
ou no host.

Referências: [RFC 6750](https://www.rfc-editor.org/rfc/rfc6750.html),
[RFC 8725](https://www.rfc-editor.org/rfc/rfc8725.html) e
[RFC 9068](https://www.rfc-editor.org/rfc/rfc9068.html).
