param([Parameter(Mandatory = $true)][string]$WasiSdkPath)
$ErrorActionPreference = 'Stop'
$taskClang = Join-Path $WasiSdkPath 'bin/clang.exe'
if (-not (Test-Path -LiteralPath $taskClang)) { throw "Compiler not found: $taskClang" }
Push-Location (Join-Path $PSScriptRoot 'grammar')
try {
    & npm.cmd ci
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
    & npm.cmd run generate
    if ($LASTEXITCODE -ne 0) { throw 'Grammar generation failed' }
    New-Item -ItemType Directory -Path (Join-Path $PSScriptRoot 'grammars') -Force | Out-Null
    & $taskClang -fPIC -shared -nostdlib -Os '-Wl,--no-entry' '-Wl,--export=tree_sitter_typhon' -I src src/parser.c -o (Join-Path $PSScriptRoot 'grammars/typhon.wasm')
    if ($LASTEXITCODE -ne 0) { throw 'WASM compilation failed' }
    & npm.cmd test
    if ($LASTEXITCODE -ne 0) { throw 'Grammar checks failed' }
} finally { Pop-Location }
