param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$File
)

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
& "$Root\.venv\Scripts\python.exe" -m typhon run $File
