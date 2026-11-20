# TYP-004 — contratos de payload

`typhon.validation` valida valores Typhon, inclusive o resultado de
`Request.json()`. Os schemas são valores reutilizáveis e imutáveis.
O checker conhece o tipo `Schema`. O runtime verifica dados dinâmicos
sem converter strings em números ou inteiros em floats.

| Construtor | Contrato |
| --- | --- |
| `text(min=0, max=8388608)` | Texto UTF-8, comprimento em pontos de código Unicode |
| `integer(min=None, max=None)` | Inteiro int64, extremos inclusivos |
| `number(min=None, max=None)` | Inteiro ou float finito, extremos inclusivos |
| `boolean()` | Bool |
| `nullable(schema)` | `None` ou valor aceito pelo schema |
| `array(schema, min=0, max=8388608)` | Lista com schema por elemento e limites de comprimento |
| `enumeration(values)` | Uma das strings declaradas, com comparação exata |
| `object(required, optional={}, allow_extra=False)` | Dicionário com schemas por chave e permissão opcional de campos desconhecidos |

Passe opções posicionalmente. Campos opcionais podem faltar. Para aceitar
`None` em um campo presente, envolva seu schema em `nullable()`. O parâmetro
`allow_extra` permite dados adicionais e deixa o payload original disponível.
Use o valor já analisado para construir o DTO que será usado pela aplicação.

`check(schema, value) -> list[dict[str, str]]` devolve uma lista vazia
quando o valor satisfaz o contrato. Cada diagnóstico contém `path`, `rule`,
`code` e `message`. Os paths usam a declaração do schema e índices de
array, por exemplo `items[2].quantity`. Campos extras são reportados no
objeto pai para evitar que uma chave fornecida pelo cliente exponha dados.
O diagnóstico de campos segue a ordem de declaração.

Há até 64 diagnósticos por chamada, 256 campos por objeto de schema,
100 mil passos, profundidade 64 e paths de até 512 bytes. O loop consulta
o contexto e consome o orçamento da VM. Limites de trabalho, profundidade
ou path retornam `ValidationError`, para o chamador decidir como reportar
uma validação incompleta.

O [exemplo completo de cadastro](../examples/payload_validation.ty)
responde 201 para um DTO aceito, 400 quando `Request.json()` rejeita o
corpo e 422 quando o corpo é JSON válido e viola o schema. A biblioteca
tem uso independente de HTTP. Os construtores oferecem os dados de
estrutura que um futuro gerador OpenAPI precisará representar.
