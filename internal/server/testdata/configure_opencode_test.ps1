$ErrorActionPreference = 'Stop'

function Assert-Equal([object] $Expected, [object] $Actual, [string] $Label) {
    if ($Expected -cne $Actual) {
        throw "$Label did not match. Expected '$Expected', got '$Actual'."
    }
}

function Run-SetupTest([string] $GatewayResponse, [string] $Key) {
    $child = [PowerShell]::Create()
    try {
        $origin = 'http://127.0.0.1:' + $listener.LocalEndpoint.Port
        $payload = @{
            origin = $origin
            plan = @{ id = 'gpt-plan'; group = 'GPT' }
            build = @{ id = 'claude-build'; group = 'Claude' }
        } | ConvertTo-Json -Compress
        $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($payload))
        [IO.File]::WriteAllText($download, $template.Replace($placeholder, $encoded), $bomEncoding)

        $wrapper = {
            param($ScriptPath, $Key)
            function Read-Host {
                param($Prompt, [switch] $AsSecureString)
                if (-not $AsSecureString) { throw 'The script must read the key without echoing it.' }
                ConvertTo-SecureString $Key -AsPlainText -Force
            }
            $global:LASTEXITCODE = 0
            & $ScriptPath
            $global:LASTEXITCODE
        }
        [void] $child.AddScript($wrapper.ToString()).AddArgument($download).AddArgument($Key)
        $completed = $child.BeginInvoke()
        $pending = $listener.BeginAcceptTcpClient($null, $null)
        if (-not $pending.AsyncWaitHandle.WaitOne(10000)) {
            $errors = ($child.Streams.Error | ForEach-Object { $_.Exception.Message }) -join ' '
            throw "No request reached the fake gateway. $errors"
        }
        $client = $listener.EndAcceptTcpClient($pending)
        try {
            $stream = $client.GetStream()
            $stream.ReadTimeout = 10000
            $reader = New-Object IO.StreamReader($stream)
            Assert-Equal 'GET /v1/models HTTP/1.1' ($reader.ReadLine()) 'Model catalog request'
            $authorization = $null
            while ($line = $reader.ReadLine()) {
                if ($line.StartsWith('Authorization:', [StringComparison]::OrdinalIgnoreCase)) {
                    $authorization = $line.Substring(14).Trim()
                }
            }
            Assert-Equal "Bearer $Key" $authorization 'API Key header'
            $bytes = [Text.Encoding]::UTF8.GetBytes($GatewayResponse)
            $headers = "HTTP/1.1 200 OK" + [char]13 + [char]10 +
                "Content-Type: application/json" + [char]13 + [char]10 +
                "Content-Length: $($bytes.Length)" + [char]13 + [char]10 +
                'Connection: close' + [char]13 + [char]10 + [char]13 + [char]10
            $headerBytes = [Text.Encoding]::ASCII.GetBytes($headers)
            $stream.Write($headerBytes, 0, $headerBytes.Length)
            $stream.Write($bytes, 0, $bytes.Length)
        } finally {
            $client.Dispose()
        }
        if (-not $completed.AsyncWaitHandle.WaitOne(10000)) {
            throw 'The configurator did not finish.'
        }
        $output = @($child.EndInvoke($completed))
        return @{
            exitCode = $output[-1]
            origin = $origin
            errors = ($child.Streams.Error | ForEach-Object { $_.Exception.Message }) -join ' '
        }
    } finally {
        $child.Dispose()
    }
}

$prefix = Join-Path ([IO.Path]::GetTempPath()) ('configure-opencode-test-' + [Guid]::NewGuid().ToString('N'))
[void] [IO.Directory]::CreateDirectory($prefix)
$listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
$listener.Start()
$originalEnv = @{
    OPENCODE_CONFIG = $env:OPENCODE_CONFIG
    OPENCODE_CONFIG_CONTENT = $env:OPENCODE_CONFIG_CONTENT
    LOCALAPPDATA = $env:LOCALAPPDATA
}
try {
    $asset = Join-Path $PSScriptRoot '..\assets\configure-opencode.ps1'
    $asset = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($asset)
    $template = [IO.File]::ReadAllText($asset)
    $placeholder = '__OPENCODE_SETUP_PAYLOAD__'
    Assert-Equal 1 ([regex]::Matches($template, $placeholder).Count) 'Payload placeholder count'
    $bomEncoding = New-Object Text.UTF8Encoding($true)
    $download = Join-Path $prefix 'download.ps1'
    $configPath = Join-Path $prefix 'opencode.jsonc'
    $env:OPENCODE_CONFIG = $configPath
    $env:OPENCODE_CONFIG_CONTENT = $null
    $env:LOCALAPPDATA = Join-Path $prefix 'AppData'
    $original = @'
{
  // Existing settings can use JSONC.
  "model": "other/active",
  "other": { "literal": "https://example.test/with//slashes/*ok*/", },
  "agents": { "plan": { "description": "keep me", }, },
  "providers": { "other": { "settings": { "baseURL": "https://example.test/v1" } }, },
}
'@
    [IO.File]::WriteAllText($configPath, $original)

    $rejected = Run-SetupTest '{"data":[{"id":"gpt-plan"},{"id":"Claude-build"}]}' 'test-invalid-version-key'
    Assert-Equal 1 $rejected.exitCode 'Case-sensitive model validation exit status'
    Assert-Equal $original ([IO.File]::ReadAllText($configPath)) 'Config after model rejection'
    Assert-Equal $false ([IO.Directory]::Exists($env:LOCALAPPDATA)) 'Secrets after model rejection'

    $configured = Run-SetupTest '{"data":[{"id":"gpt-plan"},{"id":"claude-build"}]}' 'test-success-key'
    Assert-Equal 0 $configured.exitCode "Success exit status $($configured.errors)"
    $newSource = [IO.File]::ReadAllText($configPath)
    $config = $newSource | ConvertFrom-Json
    $backup = @(Get-ChildItem -LiteralPath $prefix -Filter 'opencode.jsonc.bak-*')
    Assert-Equal 1 $backup.Count 'Config backup count'
    Assert-Equal $original ([IO.File]::ReadAllText($backup[0].FullName)) 'Backup content'
    Assert-Equal 'other/active' $config.model 'Global model'
    Assert-Equal 'keep me' $config.agents.plan.description 'Existing agent settings'
    Assert-Equal 'https://example.test/with//slashes/*ok*/' $config.other.literal 'String containing comment markers'
    Assert-Equal 'https://example.test/v1' $config.providers.other.settings.baseURL 'Existing provider'
    Assert-Equal 'gpt-plan' $config.agents.plan.model.model 'Plan model'
    Assert-Equal 'claude-build' $config.agents.build.model.model 'Build model'
    $provider = $config.providers.PSObject.Properties[$config.agents.plan.model.providerID].Value
    Assert-Equal '@opencode/ai/providers/openai-compatible/responses' $provider.package 'Plan adapter'
    Assert-Equal ($configured.origin + '/v1') $provider.settings.baseURL 'Plan endpoint'
    $secretPath = $provider.settings.apiKey -replace '^\{file:(.+)\}$', '$1'
    Assert-Equal 'test-success-key' ([IO.File]::ReadAllText($secretPath)) 'Local API Key file'
    Assert-Equal $false $newSource.Contains('test-success-key') 'API Key outside configuration'
    $provider = $config.providers.PSObject.Properties[$config.agents.build.model.providerID].Value
    Assert-Equal '@opencode/ai/providers/anthropic' $provider.package 'Build adapter'
    Assert-Equal ($configured.origin + '/v1') $provider.settings.baseURL 'Build endpoint'

    if ($env:OPENCODE_TEST_CLI) {
        $cliModels = (& $env:OPENCODE_TEST_CLI models --standalone | Out-String)
        Assert-Equal 0 $LASTEXITCODE 'OpenCode models exit status'
        if (-not $cliModels.Contains("$($config.agents.plan.model.providerID)/gpt-plan") -or
            -not $cliModels.Contains("$($config.agents.build.model.providerID)/claude-build")) {
            throw 'OpenCode CLI did not load both configured models.'
        }
    }

    $lock = [IO.File]::Open($configPath, 'Open', 'Read', 'Read')
    try {
        $failedWrite = Run-SetupTest '{"data":[{"id":"gpt-plan"},{"id":"claude-build"}]}' 'test-failed-write-key'
        Assert-Equal 1 $failedWrite.exitCode 'Config file sharing error exit status'
    } finally {
        $lock.Dispose()
    }
    Assert-Equal $newSource ([IO.File]::ReadAllText($configPath)) 'Config after write failure'
    Assert-Equal 'test-success-key' ([IO.File]::ReadAllText($secretPath)) 'API Key after write failure'
    Assert-Equal 1 @(Get-ChildItem -Path $env:LOCALAPPDATA -Recurse -File).Count 'API Key rollback file count'
    Write-Output "OpenCode configurator regression tests passed on PowerShell $($PSVersionTable.PSVersion)."
} finally {
    $listener.Stop()
    foreach ($name in $originalEnv.Keys) {
        [Environment]::SetEnvironmentVariable($name, $originalEnv[$name], 'Process')
    }
    [IO.Directory]::Delete($prefix, $true)
}
