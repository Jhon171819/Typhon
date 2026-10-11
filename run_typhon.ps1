param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$File
)

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Push-Location $Root
try {
    go run ./cmd/typhon run $File
}
finally {
    Pop-Location
}
