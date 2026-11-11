[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is not on PATH. Install a Go version compatible with services/vico-bridge/go.mod.'
}
$module = Join-Path (Split-Path -Parent $PSScriptRoot) 'services\vico-bridge'
$binaryDirectory = Join-Path $module 'bin'
New-Item -ItemType Directory -Force -Path $binaryDirectory | Out-Null
Push-Location $module
try {
    go test -p 2 ./cmd/devices
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
    go build -p 2 -o (Join-Path $binaryDirectory 'vico-cli.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
    Write-Host "Built $(Join-Path $binaryDirectory 'vico-cli.exe')"
}
finally {
    Pop-Location
}
