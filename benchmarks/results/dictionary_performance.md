# Dicionários e drivers: verificação de 2026-10-31

Medições locais em Windows/amd64, Go 1.26.2, Intel Core i5-7400.
Comparação entre os fontes originais de HEAD e os fontes corrigidos, usando
o mesmo `dictionary_benchmark_test.go`. Cada resultado é a mediana de três
execuções com `-benchtime=500ms`, sem profiling. Os tempos variam com GC e
carga da máquina; os volumes abaixo são alocações cumulativas, não pico de RAM.

| Caso | Antes | Depois | Alocações antes → depois |
| --- | ---: | ---: | --- |
| Percorrer 10 mil registros com `row['id']` | 10,74 ms | 10,06 ms | 1.760 → 1.760 bytes; 6 → 6 alocações |
| Percorrer 10 mil registros com `row.get('id', 0)` | 22,15 ms | 16,76 ms | 16,72 → 9,76 MB; 90.007 → 30.007 alocações |
| Buscar uma chave em dicionário tipado, sem o loop da VM | 217 ns | 170 ns | 0 → 0 alocações |
| Validar pela primeira vez 10 mil `dict[str, int \| str]` | 4.904 ms | 18,99 ms | 3.106 → 15,02 MB; cerca de 580 mil → 60.103 alocações |

Os testes de leitura somam `id` em `list[dict[str, int]]`, com três campos por
registro e contratos já instalados. Excluem parsing, compilação, montagem dos
dados e primeira validação. O teste de primeira validação usa um campo por
registro e inclui instalação dos contratos e conversões necessárias. Seu ganho
de aproximadamente 258 vezes não representa o ganho em cada leitura subsequente.

## Causas e correções

- Cada alternativa de uma união copiava todos os registros de validação
  anteriores. Em arrays de dicionários com valores `int | str`, isso tornava a
  primeira validação quadrática. Agora um registro de alterações permite desfazer
  somente os contratos propostos pela alternativa que falhou. O commit continua
  acontecendo apenas depois da validação completa.
- As implementações dos métodos de dicionário agora são compartilhadas.
  Cada método vinculado continua preservando seu próprio receptor.
- Argumentos de métodos nativos são montados em uma única alocação.
- Tipos sem contratos de coleção são validados diretamente, e os mapas do plano
  de validação são criados apenas quando necessários.

As buscas já usavam uma tabela hash, com custo médio O(1). Ainda há custo da VM,
checagens de tipo e chamadas de método. `.get()` continua alocando por chamada;
`d['chave']` é mais barato quando a chave é obrigatória. Remover uma chave continua
O(n), e `keys()`, `values()` e `items()` materializam snapshots. Esta alteração
não elimina esses custos nem garante eficiência máxima para todo workload.

## Validação dos drivers

- HTTP: cliente e servidor reais em loopback, timeout, cancelamento, limite de
  corpo e recuperação após falha do handler passaram.
- PostgreSQL: integração com PostgreSQL 16.15 local passou, incluindo parâmetros,
  conversões, transações, limpeza de recursos e timeout.
- Redis: operações, TTL e erros passaram usando `miniredis`, um servidor RESP de
  teste. A integração opcional com Redis real não foi executada.
- Os testes verificam que importar os três drivers nativos não inicia Python.

Para repetir as medições:

```powershell
go test ./internal/typhon -run '^$' -bench '^BenchmarkDictionary' -benchmem -benchtime=500ms -count=3
```

Os testes de regressão cobrem rollback de alternativas de união, preservação de
aliases e contratos anteriores, e métodos vinculados a dicionários diferentes.
