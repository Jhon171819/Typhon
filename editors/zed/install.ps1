$ErrorActionPreference = 'Stop'
$taskWasm = Join-Path $PSScriptRoot 'grammars/typhon.wasm'
if (-not (Test-Path -LiteralPath $taskWasm)) { throw 'Build the grammar with build.ps1 first, or use the prebuilt ZIP.' }
$taskDestination = Join-Path $env:LOCALAPPDATA 'Zed/extensions/installed/typhon'
if (Test-Path -LiteralPath $taskDestination) {
    $taskOldManifest = Get-Content -LiteralPath (Join-Path $taskDestination 'extension.toml') -Raw
    if ($taskOldManifest -notmatch '(?m)^id = "typhon"$') { throw "Unrecognized extension at $taskDestination" }
}
# Keep a local grammar Git snapshot so Zed's dev rebuild can fetch real sources
# even while these changes have not been published to the project repository.
$taskSnapshot = Join-Path $env:LOCALAPPDATA 'Zed/extensions/work/typhon-grammar'
New-Item -ItemType Directory -Path (Join-Path $taskSnapshot 'src') -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'grammar/grammar.js') -Destination $taskSnapshot
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'grammar/src/parser.c') -Destination (Join-Path $taskSnapshot 'src')
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'grammar/src/tree_sitter') -Destination (Join-Path $taskSnapshot 'src') -Recurse -Force
& git -C $taskSnapshot init --quiet
if ($LASTEXITCODE -ne 0) { throw 'Cannot initialize grammar snapshot' }
& git -C $taskSnapshot add grammar.js src
if ($LASTEXITCODE -ne 0) { throw 'Cannot stage grammar snapshot' }
& git -C $taskSnapshot diff --cached --quiet
if ($LASTEXITCODE -eq 1) {
    & git -C $taskSnapshot -c user.name='Typhon editor build' -c user.email='typhon-build@localhost' commit --quiet -m 'Snapshot Typhon editor grammar'
    if ($LASTEXITCODE -ne 0) { throw 'Cannot save grammar snapshot' }
} elseif ($LASTEXITCODE -ne 0) { throw 'Cannot inspect grammar snapshot' }
$taskRevision = (& git -C $taskSnapshot rev-parse HEAD).Trim()
$taskRepository = ([System.Uri]($taskSnapshot.Replace('\', '/') + '/')).AbsoluteUri
New-Item -ItemType Directory -Path (Join-Path $taskDestination 'grammars') -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'languages') -Destination $taskDestination -Recurse -Force
Copy-Item -LiteralPath $taskWasm -Destination (Join-Path $taskDestination 'grammars/typhon.wasm') -Force
$taskManifest = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'extension.toml') -Raw
$taskManifest += "`n[grammars.typhon]`nrepository = `"$taskRepository`"`nrev = `"$taskRevision`"`n"
[System.IO.File]::WriteAllText((Join-Path $taskDestination 'extension.toml'), $taskManifest)
Write-Output "Installed Typhon in $taskDestination. Restart Zed and open a .ty file."
