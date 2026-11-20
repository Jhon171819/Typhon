# Distribuição Typhon 0.2.0-dev

Os pacotes contêm o executável nativo, exemplos, documentação, licença do Typhon
e licenças das dependências incorporadas. Não é necessário instalar Go ou Python
para executar programas que usam apenas a VM e os módulos nativos.

## Escolher o pacote

| Pacote | Destino | Executável |
| --- | --- | --- |
| `typhon-0.2.0-dev-windows-amd64.zip` | Windows x64 | `typhon.exe` |
| `typhon-0.2.0-dev-linux-amd64.tar.gz` | Linux x64 | `typhon` |
| `typhon-0.2.0-dev-linux-arm64.tar.gz` | Linux ARM64 | `typhon` |
| `typhon-0.2.0-dev-source.zip` | Código atual do projeto | Compilação com Go 1.26+ |

Todos os binários são compilados com `CGO_ENABLED=0`, `-trimpath` e
`-ldflags="-s -w"`. A distribuição usa o estado atual dos arquivos do projeto;
`BUILD_INFO.json` registra a versão, revisão Git, presença de alterações locais,
compilador, plataforma e hash do executável. A versão ainda é de desenvolvimento.

## Windows

Extraia o ZIP e abra o PowerShell na pasta extraída:

```powershell
.\typhon.exe version
.\typhon.exe run examples\vm_safety.ty
.\typhon.exe check examples\user_service.ty
.\typhon.exe run examples\user_service.ty
```

Para executar sua aplicação, copie seus arquivos `.ty` e módulos locais para
uma pasta acessível e informe o caminho do arquivo principal:

```powershell
.\typhon.exe run C:\app\main.ty
```

## Linux

Escolha a arquitetura correspondente (`uname -m`: `x86_64` → amd64;
`aarch64` → arm64). O TAR preserva a permissão de execução:

```bash
tar -xzf typhon-0.2.0-dev-linux-amd64.tar.gz
cd typhon-0.2.0-dev-linux-amd64
./typhon version
./typhon run examples/vm_safety.ty
./typhon run examples/user_service.ty
./typhon run /app/main.ty
```

É possível copiar apenas `typhon` e os arquivos da aplicação para um container
Linux da mesma arquitetura. HTTPS/TLS precisa dos certificados CA da imagem
base. PostgreSQL e Redis são serviços externos; seus drivers já estão no binário.
Para publicar um servidor HTTP em container, o código deve escutar em
`0.0.0.0:8080`, em vez de apenas `127.0.0.1:8080`. O exemplo incluído usa loopback
por padrão. Esta distribuição não cria nem inclui uma imagem Docker.

## Exemplos e dependências opcionais

Os exemplos `user_service.ty`, `vm_safety.ty`,
`main_script.ty`, `parallel_task.ty` e `void_return.ty` rodam no caminho nativo.
`http_server.ty` serve `/health` e `/echo`; `http_client.ty` usa a variável
`HTTP_URL`, ou `http://127.0.0.1:8080/health` quando ela não está definida.
`postgres.ty` usa `DATABASE_URL`; `redis.ty` usa `REDIS_URL`.
Veja `docs/native_drivers.md` para os contratos desses módulos.

`hello_world.ty` e os exemplos que importam bibliotecas Python exigem Python
3.11+ e os respectivos
pacotes. O worker da ponte está incorporado ao executável, mas o interpretador
Python não está. Informe `TYPHON_PYTHON` para selecionar o interpretador.
Não ative `--warm-python` em ambientes que não tenham Python.

`invalid_missing_types.ty` e `runtime_type_error.ty` são exemplos intencionalmente
inválidos. O checker deve rejeitá-los; eles não são testes de instalação.

## Conferir integridade

Na pasta `dist`, `SHA256SUMS.txt` contém os hashes dos arquivos compactados.
Dentro de cada pacote, outro `SHA256SUMS.txt` cobre seu conteúdo.

```powershell
Get-FileHash .\typhon-0.2.0-dev-windows-amd64.zip -Algorithm SHA256
```

Compare o resultado com a linha correspondente de `SHA256SUMS.txt`. No Linux:

```bash
sha256sum -c SHA256SUMS.txt
```

## Compilar o código-fonte

O pacote de fontes inclui os arquivos atuais do repositório, inclusive alterações
locais, sem `.git`, ambientes virtuais, caches, distribuições anteriores ou
binários de benchmark. Ele não contém dependências Go vendorizadas: o primeiro
build precisa acessar o proxy Go ou um cache de módulos já preenchido.

```powershell
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go build -mod=readonly -trimpath -ldflags='-s -w' -o typhon ./cmd/typhon
```

Troque `GOOS` para `windows` e a saída para `typhon.exe`, ou `GOARCH` para
`arm64`, conforme o destino. Abra outro terminal para voltar ao ambiente de
compilação padrão.
