# TYP-003 — JSON nativo

`import json`, `from json import loads, dumps` e o alias histórico
`py.json` usam o mesmo codec Go. O runtime oferece `loads(text: str) -> Any`
e `dumps(value: Any) -> str`. A versão anterior oferecia apenas serialização.
O decoder padrão fornece os tokens. O codec monta os valores Typhon
diretamente com o token numérico original para validar int64 antes da conversão.

| JSON | Typhon |
| --- | --- |
| `null` | `None` |
| `true`, `false` | `bool` |
| Número sem fração ou expoente | `int` de 64 bits, com erro fora da faixa |
| Número com fração ou expoente | `float` finito, com arredondamento float64 |
| String Unicode | `str` em UTF-8 |
| Array | `list` |
| Objeto | `dict` com chaves `str` e ordem de inserção |

`loads` aceita um único valor JSON. Rejeita chaves duplicadas após
decodificar escapes, bytes UTF-8 inválidos e escapes substitutos sem par.
`dumps` preserva a representação textual existente para números e a
conversão de chaves escalares em strings. Colisões após conversão e floats
não finitos geram erro. Se um float integral for serializado sem ponto ou
expoente, `loads` interpreta o token como inteiro.

As operações aplicam, separadamente do limite de transporte HTTP, 8 MiB de
entrada ou saída, profundidade máxima de 64 a partir da raiz e 100 mil nós
incluindo chaves. Cada nó também consome uma instrução do orçamento da VM.
O codec consulta o contexto durante a análise e a conversão. Ciclos em
coleções são interrompidos pelo limite de profundidade.

Falhas geram `ValidationError`. `err.offset` é uma posição em bytes
a partir de 1 próxima ao erro de entrada, ou `None` em falhas de
serialização. O host Go também pode recuperar `*typhon.JSONError` por
`errors.As`. Os diagnósticos incluem a razão e a posição sem incorporar
texto do payload. Cancelamento e esgotamento do orçamento da VM seguem o
contrato de erros obrigatórios da VM.

`req.json()` decodifica `req.body` com as regras de `json.loads`.
`http.json(status, value)` serializa com as regras de `json.dumps` e
define Content-Type JSON. Um corpo acima do limite HTTP recebe 413 antes
do handler. Um valor que excede o limite de nós gera `ValidationError`
dentro do handler, que pode mapeá-lo para 400.

```typhon
import typhon.http as http

def handle(req: http.Request) -> http.Response:
    try:
        return http.json(200, req.json())
    except ValidationError:
        return http.json(400, {'error': 'invalid JSON'})
```

Execute [native_json.ty](../examples/native_json.ty) para ver a precisão
de int64 e o diagnóstico de uma chave duplicada.
