# TYP-008 — módulos Typhon

O runtime anterior carregava módulos Typhon em `from ... import` e
encaminhava `import ...` à ponte Python. Agora as duas sintaxes usam
a mesma resolução de módulos e o mesmo cache por Engine.

```typhon
import app.routes as routes
from services.profiles import lookup as profile_name
from config import DefaultName as DisplayName
```

Para um módulo local, o checker resolve funções, classes, campos exportados
e aliases importados com `from`. Um alias de tipo é utilizável em anotações.
`import pkg.mod as alias` oferece acesso aos membros em `alias.member`.
Em nomes com ponto, use um alias explícito para nomear o módulo carregado.

## Busca

1. Módulos nativos reservados, como `typhon.http` e `json`, têm prioridade.
2. `py.<nome>` seleciona Python explicitamente, incluindo quando existe um
   arquivo Typhon com o mesmo nome.
3. Cada nome local é uma sequência de identificadores separados por ponto.
   O loader procura `pkg/mod.ty` e depois `pkg/mod/__init__.ty`. Procura
   primeiro no diretório do arquivo que importa e depois na raiz do projeto.
4. Por compatibilidade, o comando `run` tenta Python se o nome local não
   existir. Para continuar selecionando Python em caso de colisão, use
   `py.<nome>`. Erros de um arquivo encontrado saem com módulo e linha.

O diretório do primeiro arquivo de entrada é a raiz padrão.
`TYPHON_MODULE_ROOT` seleciona outra raiz explicitamente. A leitura
de imports mantém os caminhos dentro dessa raiz, inclusive ao resolver
links simbólicos. Um host Go ainda pode fornecer um arquivo de entrada
explícito a `Engine.LoadModule`. Use uma raiz comum configurada se esses
arquivos precisarem compartilhar dependências locais.

Cada Engine mantém o programa compilado e os globais por caminho
canônico. O módulo inicializa uma vez por Engine. Workers HTTP mantêm
seus próprios globais e seus próprios caches. Uma importação de módulo
consulta os membros atuais. `from` vincula o valor exportado no momento
da importação. Ciclos, símbolos ausentes e contratos inválidos são
diagnosticados com a linha da importação.

Essa busca usa diretórios do projeto. Mantenha dependências versionadas
no repositório ou em uma raiz explicitamente montada. A resolução remota,
manifestos e lockfiles pertencem ao futuro gerenciador de pacotes.

## Exemplo e verificação

```powershell
go run ./cmd/typhon check-imports examples/modular_app/main.ty
go run ./cmd/typhon run examples/modular_app/main.ty
```

`check-imports` compila o grafo e exige uma dependência local, nativa
ou explicitamente identificada por `py.`. A checagem da ponte Python
acontece em execução. O [exemplo modular](../examples/modular_app)
tem entrada, pacote `app`, rotas, serviços, repositório e configuração.
`GET /?name=Ada` retorna nome e versão, e `GET /health` retorna 200.
