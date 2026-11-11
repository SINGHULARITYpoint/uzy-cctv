[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$private = Join-Path $root 'local\private\restream'
$inventory = Join-Path $root 'local\private\store-device-list-powered.json'
$template = Join-Path $root 'infra\go2rtc.template.yaml'
$devices = @(Get-Content -LiteralPath $inventory -Raw | ConvertFrom-Json)
$names = @('Dining Area','Cash Register','Kitchen Back')
$serials = @{}
foreach ($name in $names) {
    $matching = @($devices | Where-Object deviceName -eq $name)
    if ($matching.Count -ne 1 -or $matching[0].serialNumber -notmatch '^[A-Za-z0-9_-]+$') {
        throw "Expected exactly one safe serial for $name in the private inventory."
    }
    $serials[$name] = $matching[0].serialNumber
}

$email = $env:VICOHOME_EMAIL
$password = $env:VICOHOME_PASSWORD
if (-not $email) { $email = Read-Host 'Dedicated bridge account email' }
if (-not $password) {
    $secure = Read-Host 'Dedicated bridge account password' -AsSecureString
    $ptr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try { $password = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr) }
}
if ($email -match '[\r\n"]' -or $password -match '[\r\n"]') {
    throw 'Account values contain a character unsupported by the systemd environment template.'
}

New-Item -ItemType Directory -Force -Path $private | Out-Null
$yaml = (Get-Content -LiteralPath $template -Raw).
    Replace('__DINING__',$serials['Dining Area']).
    Replace('__CASH__',$serials['Cash Register']).
    Replace('__KITCHEN__',$serials['Kitchen Back'])
$yaml | Set-Content -LiteralPath (Join-Path $private 'go2rtc.yaml') -Encoding utf8
@(
    "VICOHOME_EMAIL=$email"
    "VICOHOME_PASSWORD=$password"
    'VICOHOME_CACHE_DIR=/home/cameraops/.config/uzy-cctv/cache'
) | Set-Content -LiteralPath (Join-Path $private 'bridge.env') -Encoding utf8
Write-Host "Prepared ignored restream files under $private"
