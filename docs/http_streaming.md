# TYP-019 — uploads e respostas HTTP por streaming

Após registrar a rota, habilite
`routes.stream_upload(method, pattern, max_bytes, max_part_bytes?, max_parts?)`.
Os limites são por requisição. O máximo configurável é 128 MiB e
100 partes. O padrão de `max_part_bytes` é o limite total e o padrão
de `max_parts` é 16. As demais rotas mantêm o limite de 8 MiB para
corpo em memória e suas APIs de leitura atuais.

Em uma rota configurada, `req.stream()` abre um `http.Body` bruto,
ou `req.multipart()` abre um `http.Multipart`. Escolha um modo
uma vez por requisição. O campo `req.body` fica vazio nessas rotas.
`form.next()` devolve a próxima `http.Part` ou `None`. Cada parte expõe
`name`, `filename` e `content_type`. O nome do arquivo é apenas
metadata. Valores com separadores de caminho, caracteres de controle
ou segmentos de travessia recebem 400. Gere a chave de armazenamento
na aplicação.

`body.read(n)` e `part.read(n)` devolvem até 65.536 bytes como
`scalars.Bytes`, ou `None` no fim. `part.close()` descarta o
restante dentro do limite da parte. `form.next()` também esgota a
parte anterior. Fechar o formulário ou o corpo encerra sua API de
leitura. Um excesso de bytes ou partes detectado na leitura recebe 413.
Os uploads seguem diretamente do socket para os blocos, sem
arquivos temporários. Após uma recusa antecipada, o servidor pode
descartar até 2 MiB do restante, durante até 150 ms, para entregar
a resposta antes de fechar a conexão HTTP/1. Um cliente que continue
escrevendo pode observar o encerramento da conexão.

`http.stream(status, producer, state, headers?)` devolve uma
resposta cujo produtor tem assinatura `(Any) -> scalars.Bytes | None`.
O produtor devolve um bloco de 1 a 65.536 bytes a cada chamada e
`None` quando termina. Passe estado próprio da requisição, inclusive
um leitor aberto no handler. A VM espera a escrita e o flush no
socket antes de pedir o próximo bloco. HEAD envia apenas os headers.
Use `Content-Type` e `Content-Disposition` apropriados e deixe o
framing de transferência com o servidor.

Falhas antes do primeiro bloco ainda podem retornar um problema HTTP.
Após o primeiro bloco o status já foi enviado. A VM interrompe a
transferência, registra a falha com o ID da requisição e fecha os
recursos da requisição. O cliente deve tratar download truncado como
incompleto. Prazo da requisição e orçamento de bytes da VM continuam
valendo. Os 128 MiB são o teto do transporte, sujeito a esses orçamentos.
Escrever e ler por blocos limita a memória retida, mas a quota lógica
cumulativa continua contando alocações e bytes de saída.

Execute [`http_streaming.ty`](../examples/http_streaming.ty) para
servir `/v1/uploads` e `/v1/export`. Para simular encaminhamento
para armazenamento externo, execute [`http_stream_sink.ty`](../examples/http_stream_sink.ty)
em outro processo e configure a URL fixa
`STREAM_SINK_URL=http://127.0.0.1:8082/chunks` no primeiro. O sink
valida e confirma cada bloco Base64 de teste sem persistência.
Ao integrar um armazenamento real, associe os blocos a uma sessão
de upload e só publique o objeto depois de aceitar o corpo completo.

```sh
curl -F 'file=@payload.bin' http://127.0.0.1:8080/v1/uploads
curl -o export.csv http://127.0.0.1:8080/v1/export
go test ./internal/typhon -run '^$' -bench '^BenchmarkNativeHTTPStreamingLiveHeap$' -benchtime 1x
```

Em Windows amd64, a leitura sintética de 4, 16 e 40 MiB reteve
aproximadamente 160 KB incrementais em cada tamanho. O benchmark
mede heap vivo após GC em amostras por MiB com um leitor sintético.
