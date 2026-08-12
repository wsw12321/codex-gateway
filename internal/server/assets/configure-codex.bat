@echo off
setlocal
set "CODEX_SETUP_FILE=%~f0"
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "$source = Get-Content -LiteralPath $env:CODEX_SETUP_FILE -Raw; Invoke-Expression (($source -split '(?m)^:POWERSHELL\r?$', 2)[1])"
if errorlevel 1 goto failed
echo.
pause
exit /b 0

:failed
echo.
echo Codex configuration failed.
pause
exit /b 1

:POWERSHELL
$ErrorActionPreference = 'Stop'
$GatewayBaseURL = '__CODEX_GATEWAY_BASE_URL__'.TrimEnd('/')
if ($GatewayBaseURL -notmatch '^https?://') {
    throw 'Gateway URL must start with http:// or https://'
}

if ($env:CODEX_HOME) {
    $configDir = $env:CODEX_HOME
} else {
    $configDir = Join-Path $HOME '.codex'
}
$configPath = Join-Path $configDir 'config.toml'
New-Item -ItemType Directory -Force -Path $configDir | Out-Null

if (Test-Path -LiteralPath $configPath -PathType Leaf) {
    Copy-Item -LiteralPath $configPath -Destination ($configPath + '.bak') -Force
    $content = Get-Content -LiteralPath $configPath -Raw
} elseif (Test-Path -LiteralPath $configPath) {
    throw ($configPath + ' is not a regular file')
} else {
    $content = ''
}

$lines = if ($content) { $content -split "\r?\n" } else { @() }
$output = New-Object 'System.Collections.Generic.List[string]'
$inGateway = $false
$seenSection = $false
$wroteProvider = $false

foreach ($line in $lines) {
    if ($inGateway) {
        if ($line -match '^\s*\[') {
            $inGateway = $false
        } else {
            continue
        }
    }
    if ($line -match '^\s*\[model_providers\.gateway\]\s*(?:#.*)?$') {
        $inGateway = $true
        continue
    }
    if (-not $seenSection -and $line -match '^\s*\[') {
        if (-not $wroteProvider) {
            $output.Add('model_provider = "gateway"') | Out-Null
            $output.Add('') | Out-Null
            $wroteProvider = $true
        }
        $seenSection = $true
    }
    if (-not $seenSection -and $line -match '^\s*model_provider\s*=') {
        if (-not $wroteProvider) {
            $output.Add('model_provider = "gateway"') | Out-Null
            $wroteProvider = $true
        }
        continue
    }
    $output.Add($line) | Out-Null
}

$result = ($output -join "`r`n").TrimEnd()
if (-not $wroteProvider) {
    if ($result) {
        $result += "`r`n`r`n"
    }
    $result += 'model_provider = "gateway"'
}
if ($result) {
    $result += "`r`n`r`n"
}
$result += @"
[model_providers.gateway]
name = "Personal Codex Gateway"
base_url = "$GatewayBaseURL"
env_key = "CODEX_GATEWAY_API_KEY"
wire_api = "responses"
env_http_headers = { "X-Codex-Project" = "CODEX_GATEWAY_PROJECT" }
request_max_retries = 2
stream_max_retries = 2
"@
$result += "`r`n"

$utf8WithoutBOM = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($configPath, $result, $utf8WithoutBOM)
Write-Host ('Codex configured: ' + $configPath)
if (Test-Path -LiteralPath ($configPath + '.bak')) {
    Write-Host ('Previous configuration backup: ' + $configPath + '.bak')
}
