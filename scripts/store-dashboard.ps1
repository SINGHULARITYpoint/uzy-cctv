[CmdletBinding(DefaultParameterSetName = 'Round')]
param(
    [Parameter(Mandatory, ParameterSetName = 'Preflight')][switch]$Preflight,
    [Parameter(Mandatory, ParameterSetName = 'KitchenAB')][switch]$KitchenAB,
    [Parameter(Mandatory, ParameterSetName = 'TwoCamera')][switch]$TwoCamera,
    [Parameter(Mandatory, ParameterSetName = 'PoweredPair')][switch]$PoweredPair,
    [Parameter(Mandatory, ParameterSetName = 'DiningSolo')][switch]$DiningSolo,
    [Parameter(Mandatory, ParameterSetName = 'ReceiverComparison')][switch]$ReceiverComparison,
    [Parameter(Mandatory, ParameterSetName = 'ThreeCamera')][switch]$ThreeCamera,
    [Parameter(Mandatory, ParameterSetName = 'CountersCompare')][switch]$CountersCompare,
    [Parameter(Mandatory, ParameterSetName = 'PktmonCompare')][switch]$PktmonCompare,
    [Parameter(Mandatory, ParameterSetName = 'Live')][switch]$Live,
    [Parameter(Mandatory, ParameterSetName = 'Round')][ValidateSet(1,2)][int]$Round,
    [Parameter(Mandatory, ParameterSetName = 'Review')][ValidateSet(1,2)][int]$Review,
    [double]$Minutes = 0,
    [switch]$LowBandwidth,
    [switch]$Direct,
    [int]$ReassemblyWindowMs,
    [int]$FrameStallMs
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$variables = 'VICOHOME_EMAIL','VICOHOME_PASSWORD','VICOHOME_CACHE_DIR','VICOHOME_DEBUG','VICO_ICE_FORCE_DIRECT','VICO_REASSEMBLY_WINDOW_MS','VICO_FRAME_STALL_MS'
$previous = @{}
foreach ($name in $variables) { $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
Push-Location -LiteralPath $root
try {
    Remove-Item Env:VICOHOME_DEBUG -ErrorAction SilentlyContinue
    if ($Direct) {
        $env:VICO_ICE_FORCE_DIRECT = '1'
    }
    if ($ReassemblyWindowMs -gt 0) {
        $env:VICO_REASSEMBLY_WINDOW_MS = "$ReassemblyWindowMs"
    }
    if ($FrameStallMs -gt 0) {
        $env:VICO_FRAME_STALL_MS = "$FrameStallMs"
    }
    if (!$Review) {
        $credentialPath = Join-Path $root 'local/private/vicohome-store.credential.xml'
        $credential = if (Test-Path -LiteralPath $credentialPath) { Import-Clixml -LiteralPath $credentialPath }
            else { Get-Credential -Message 'Dedicated VicoHome store bridge account' }
        if ($credential -isnot [pscredential]) { throw 'A dedicated store bridge credential is required.' }
        $labPath = Join-Path $root 'local/private/vicohome-lab.credential.xml'
        if ((Test-Path -LiteralPath $labPath) -and $credential.UserName -eq (Import-Clixml -LiteralPath $labPath).UserName) {
            throw 'The store and home bridge accounts must be different. No login attempted.'
        }
        $env:VICOHOME_EMAIL = $credential.UserName
        $env:VICOHOME_PASSWORD = $credential.GetNetworkCredential().Password
    } else {
        Remove-Item Env:VICOHOME_EMAIL,Env:VICOHOME_PASSWORD -ErrorAction SilentlyContinue
    }
    if ($LowBandwidth -and !$Live) {
        throw '-LowBandwidth is an opt-in store Overview trial mode and requires -Live.'
    }
    if ($CountersCompare) {
        & python scripts/counters_compare.py --wall-s 105 --baseline-dir local/store-dashboard/live-20261111-124834
        if ($LASTEXITCODE -ne 0) { throw 'Counters compare failed. See its result and private evidence.' }
        return
    }
    if ($PktmonCompare) {
        & python scripts/pktmon_compare.py --solo-s 10 --pair-s 30 --wall-s 60
        if ($LASTEXITCODE -ne 0) { throw 'PktMon compare failed. See its result and private evidence.' }
        return
    }
    [array]$option = if ($Preflight) { 'preflight' } elseif ($KitchenAB) { 'kitchen-ab' } elseif ($TwoCamera) { 'two-camera' } elseif ($PoweredPair) { 'powered-pair' } elseif ($DiningSolo) { 'dining-solo' } elseif ($ReceiverComparison) { 'receiver-comparison' } elseif ($ThreeCamera) { 'three-camera' } elseif ($Live) {
        $liveArgs = @('live')
        if ($Minutes -gt 0) { $liveArgs += '--minutes', "$Minutes" }
        if ($LowBandwidth) { $liveArgs += '--low-bandwidth' }
        $liveArgs
    } elseif ($Review) { 'review', $Review } else { 'round', $Round }
    & python scripts/store-dashboard.py @option
    if ($LASTEXITCODE -ne 0) { throw 'Store command did not pass. See its result and private evidence.' }
}
finally {
    foreach ($name in $variables) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
    Pop-Location
}
