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
$inRoot = $true
$found = $false
$replacement = 'openai_base_url = "' + $GatewayBaseURL + '"'

foreach ($line in $lines) {
    if ($inRoot -and $line -match '^\s*\[') {
        $inRoot = $false
    }
    if ($inRoot -and $line -match '^\s*openai_base_url\s*=') {
        if (-not $found) {
            $output.Add($replacement) | Out-Null
            $found = $true
        }
        continue
    }
    $output.Add($line) | Out-Null
}

if (-not $found) {
    $output.Insert(0, $replacement)
}
$result = $output -join "`r`n"
if (-not $result.EndsWith("`r`n")) {
    $result += "`r`n"
}

$utf8WithoutBOM = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($configPath, $result, $utf8WithoutBOM)
Write-Host ('Codex configured: ' + $configPath)
if (Test-Path -LiteralPath ($configPath + '.bak')) {
    Write-Host ('Previous configuration backup: ' + $configPath + '.bak')
}
