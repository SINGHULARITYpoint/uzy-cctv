[CmdletBinding()]
param(
    [string]$PublicKeyPath = (Join-Path $HOME '.ssh\id_ed25519.pub')
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$template = Join-Path $root 'infra\cloud-init.template.yaml'
$destinationDirectory = Join-Path $root 'infra\generated'
$destination = Join-Path $destinationDirectory 'cloud-init.yaml'

if (-not (Test-Path -LiteralPath $PublicKeyPath -PathType Leaf)) {
    throw "SSH public key not found: $PublicKeyPath"
}
$publicKey = (Get-Content -LiteralPath $PublicKeyPath -Raw).Trim()
if ($publicKey -notmatch '^ssh-(ed25519|rsa) [A-Za-z0-9+/=]+(?: .*)?$') {
    throw 'SSH public key has an unexpected format.'
}
$source = Get-Content -LiteralPath $template -Raw
if (-not $source.Contains('__SSH_PUBLIC_KEY__')) {
    throw 'Cloud-init template is missing the SSH public key placeholder.'
}
New-Item -ItemType Directory -Force -Path $destinationDirectory | Out-Null
$source.Replace('__SSH_PUBLIC_KEY__', $publicKey) |
    Set-Content -LiteralPath $destination -Encoding utf8
Write-Host "Generated $destination"
