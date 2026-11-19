# TYP-017 — exportação OpenAPI 3.1

Documente uma rota já registrada com
`routes.document(method, pattern, operation_id, tags, responses, body?, query?)`.
Somente essas rotas entram no contrato. Cada `operation_id` deve ser único.
Rotas sem metadados, como métricas e diagnósticos internos, continuam
ausentes do arquivo. Use o mesmo nome para parâmetros de caminho em
todos os métodos que compartilham o mesmo padrão.

`responses` é um `dict[int, validation.Schema | None]`. Associe um
schema para respostas JSON. Use `None` para códigos sem schema explícito.
Quando `routes.use_errors()` estiver habilitado, códigos de problema
conhecidos com `None` usam o envelope JSON da VM. `body` descreve
um corpo JSON obrigatório e inclui respostas 400 e 422. `query`
recebe um schema de objeto e projeta seus campos obrigatórios e
opcionais como parâmetros de query. Path params são inferidos
do padrão da rota. A segurança Bearer é inferida de
`require_auth`, e as rotas com política ou limite recebem os
status adicionais aplicáveis.

Compartilhe o mesmo objeto `validation.Schema` entre a declaração e
`web.validate` no handler, como em [`http_openapi.ty`](../examples/http_openapi.ty).
A metadata descreve a validação executada pelo handler. O schema
exportado também pode validar JSON em ferramentas compatíveis com
OpenAPI 3.1. Preserve o cuidado com formatos de entrada que as
ferramentas interpretam de forma diferente, como o literal JSON
`1.0` para um campo inteiro.

```sh
typhon openapi examples/http_openapi.ty > openapi.json
python -m openapi_spec_validator openapi.json
```

O comando exporta o global `routes` por padrão. Passe um nome
alternativo após o arquivo quando necessário. A exportação executa
a configuração do módulo, por isso providencie as variáveis
e dependências que ela usa. Coloque o `http.serve` no bloco
`if __name__ == '__main__':`. A saída do `print` da VM vai para
stderr durante a exportação para manter stdout como JSON.

`routes.configure_docs(title, version, enabled?)` informa os
metadados do contrato. O padrão de `enabled` é `False`. Ao optar
por `True`, o servidor gera o documento uma vez ao iniciar e publica
`GET /openapi.json` e `GET /docs`. Reserve esses caminhos caso
habilite a opção. A página usa ReDoc com versão fixa e verificação de
integridade. O navegador precisa de acesso ao CDN para renderizar a
documentação e sempre oferece o link para o JSON. O exemplo habilita
a interface quando `TYPHON_DOCS=1`. Mantenha a opção desabilitada
ou restrinja acesso no proxy para ambientes onde o contrato é privado.

[Especificação OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.1.html)
