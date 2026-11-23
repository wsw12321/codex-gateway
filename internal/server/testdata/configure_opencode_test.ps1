$ErrorActionPreference = 'Stop'

function Assert-Equal([object] $Expected, [object] $Actual, [string] $Label) {
    if ($Expected -cne $Actual) {
        throw "$Label did not match. Expected '$Expected', got '$Actual'."
    }
}

function Run-SetupTest([hashtable] $Payload, [string] $Key) {
    $child = [PowerShell]::Create()
    try {
        $json = ConvertTo-Json -InputObject $Payload -Compress -Depth 10
        $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($json))
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
        if (-not $completed.AsyncWaitHandle.WaitOne(10000)) {
            throw 'The configurator did not finish.'
        }
        Assert-Equal $false $listener.Pending() 'Script did not request the catalog from the gateway'
        $output = @($child.EndInvoke($completed))
        return @{
            exitCode = $output[-1]
            errors = ($child.Streams.Error | ForEach-Object { $_.Exception.Message }) -join ' '
        }
    } finally {
        $child.Dispose()
    }
}

$prefix = Join-Path ([IO.Path]::GetTempPath()) ('configure-opencode-' + [char]0x6D4B + [char]0x8BD5 + '-' + [Guid]::NewGuid().ToString('N'))
[void] [IO.Directory]::CreateDirectory($prefix)
$listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
$listener.Start()
$origin = 'http://127.0.0.1:' + $listener.LocalEndpoint.Port
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
  "other": { "literal": "https://example.test/with//slashes/*ok*/", "text": "\u6d4b\u8bd5", },
  "agents": { "plan": { "description": "keep me", }, },
  "providers": { "other": { "settings": { "baseURL": "https://example.test/v1" } }, },
}
'@
    [IO.File]::WriteAllText($configPath, $original)

    $payload = @{
        origin = $origin
        plan = @{ id = 'gpt-plan'; group = 'GPT' }
        build = @{ id = 'claude-build'; group = 'Claude' }
        models = @(
            @{ id = 'gpt-plan'; group = 'GPT' }
            @{ id = 'gpt-extra'; group = 'GPT' }
            @{ id = 'gemini-extra'; group = 'Gemini' }
            @{ id = 'claude-build'; group = 'Claude' }
            @{ id = 'claude-extra'; group = 'Claude' }
        )
    }
    $missingCatalog = $payload.Clone()
    $missingCatalog.Remove('models')
    $wrongCase = $payload.Clone()
    $wrongCase.build = @{ id = 'Claude-build'; group = 'Claude' }
    $wrongGroup = $payload.Clone()
    $wrongGroup.plan = @{ id = 'gpt-plan'; group = 'Gemini' }
    $duplicateID = $payload.Clone()
    $duplicateID.models = $payload.models + @{ id = 'gpt-plan'; group = 'Claude' }
    $ambiguousCase = $payload.Clone()
    $ambiguousCase.models = $payload.models + @{ id = 'Gpt-plan'; group = 'Gemini' }
    $malformedModel = $payload.Clone()
    $malformedModel.models = $payload.models + @{ id = '{file:private}'; group = 'GPT' }
    foreach ($badPayload in @($missingCatalog, $wrongCase, $wrongGroup, $duplicateID, $ambiguousCase, $malformedModel)) {
        $rejected = Run-SetupTest $badPayload 'test-invalid-catalog-key'
        Assert-Equal 1 $rejected.exitCode 'Invalid model catalog exit status'
        Assert-Equal $original ([IO.File]::ReadAllText($configPath)) 'Config after model rejection'
        Assert-Equal $false ([IO.Directory]::Exists($env:LOCALAPPDATA)) 'Secrets after model rejection'
    }

    $configured = Run-SetupTest $payload 'test-success-key'
    Assert-Equal 0 $configured.exitCode "Success exit status $($configured.errors)"
    $newSource = [IO.File]::ReadAllText($configPath)
    $config = $newSource | ConvertFrom-Json
    $backup = @(Get-ChildItem -LiteralPath $prefix -Filter 'opencode.jsonc.bak-*')
    Assert-Equal 1 $backup.Count 'Config backup count'
    Assert-Equal $original ([IO.File]::ReadAllText($backup[0].FullName)) 'Backup content'
    Assert-Equal 'other/active' $config.model 'Global model'
    Assert-Equal 'keep me' $config.agents.plan.description 'Existing agent settings'
    Assert-Equal 'https://example.test/with//slashes/*ok*/' $config.other.literal 'String containing comment markers'
    Assert-Equal ([string][char]0x6D4B + [char]0x8BD5) $config.other.text 'Chinese text'
    Assert-Equal 'https://example.test/v1' $config.providers.other.settings.baseURL 'Existing provider'
    Assert-Equal 'gpt-plan' $config.agents.plan.model.model 'Plan model'
    Assert-Equal 'claude-build' $config.agents.build.model.model 'Build model'
    $responsesProvider = $config.agents.plan.model.providerID
    $anthropicProvider = $config.agents.build.model.providerID
    $provider = $config.providers.PSObject.Properties[$responsesProvider].Value
    Assert-Equal '@opencode/ai/providers/openai-compatible/responses' $provider.package 'Plan adapter'
    Assert-Equal ($origin + '/v1') $provider.settings.baseURL 'Plan endpoint'
    Assert-Equal 'gemini-extra gpt-extra gpt-plan' ((@($provider.models.PSObject.Properties.Name) | Sort-Object) -join ' ') 'Complete Responses catalog'
    $secretPath = $provider.settings.apiKey -replace '^\{file:(.+)\}$', '$1'
    Assert-Equal 'test-success-key' ([IO.File]::ReadAllText($secretPath)) 'Local API Key file'
    Assert-Equal $false $newSource.Contains('test-success-key') 'API Key outside configuration'
    $provider = $config.providers.PSObject.Properties[$anthropicProvider].Value
    Assert-Equal '@opencode/ai/providers/anthropic' $provider.package 'Build adapter'
    Assert-Equal ($origin + '/v1') $provider.settings.baseURL 'Build endpoint'
    Assert-Equal 'claude-build claude-extra' ((@($provider.models.PSObject.Properties.Name) | Sort-Object) -join ' ') 'Complete Anthropic catalog'

    if ($env:OPENCODE_TEST_CLI) {
        $cliModels = (& $env:OPENCODE_TEST_CLI models --standalone | Out-String)
        Assert-Equal 0 $LASTEXITCODE 'OpenCode models exit status'
        foreach ($id in @("$responsesProvider/gpt-plan", "$responsesProvider/gpt-extra", "$responsesProvider/gemini-extra",
            "$anthropicProvider/claude-build", "$anthropicProvider/claude-extra")) {
            if (-not $cliModels.Contains($id)) { throw "OpenCode CLI did not load $id." }
        }
    }

    $lock = [IO.File]::Open($configPath, 'Open', 'Read', 'Read')
    try {
        $failedWrite = Run-SetupTest $payload 'test-failed-write-key'
        Assert-Equal 1 $failedWrite.exitCode 'Config file sharing error exit status'
    } finally {
        $lock.Dispose()
    }
    Assert-Equal $newSource ([IO.File]::ReadAllText($configPath)) 'Config after write failure'
    Assert-Equal 'test-success-key' ([IO.File]::ReadAllText($secretPath)) 'API Key after write failure'
    Assert-Equal 1 @(Get-ChildItem -Path $env:LOCALAPPDATA -Recurse -File).Count 'API Key rollback file count'

    $otherOrigin = $payload.Clone()
    $otherOrigin.origin = 'https://other.example.test'
    $other = Run-SetupTest $otherOrigin 'test-other-origin-key'
    Assert-Equal 0 $other.exitCode "Other origin exit status $($other.errors)"
    $config = [IO.File]::ReadAllText($configPath) | ConvertFrom-Json
    $otherProvider = $config.agents.plan.model.providerID
    $otherSettings = $config.providers.PSObject.Properties[$otherProvider].Value
    $otherSecretPath = $otherSettings.settings.apiKey -replace '^\{file:(.+)\}$', '$1'
    $config.providers.PSObject.Properties[$anthropicProvider].Value.settings |
        Add-Member -NotePropertyName 'timeout' -NotePropertyValue 4590
    $retainedModel = $config.providers.PSObject.Properties[$responsesProvider].Value.models.'gpt-plan'
    $retainedModel.name = 'Custom Plan Name'
    $retainedModel | Add-Member -NotePropertyName 'limit' -NotePropertyValue @{ context = 1000; output = 100 }
    [IO.File]::WriteAllText($configPath, (ConvertTo-Json -InputObject $config -Depth 20))

    $updated = @{
        origin = $origin
        plan = @{ id = 'gpt-plan'; group = 'GPT' }
        build = @{ id = 'gemini-added'; group = 'Gemini' }
        models = @(
            @{ id = 'gpt-plan'; group = 'GPT' }
            @{ id = 'gemini-added'; group = 'Gemini' }
        )
    }
    $reconfigured = Run-SetupTest $updated 'test-rotated-key'
    Assert-Equal 0 $reconfigured.exitCode "Catalog update exit status $($reconfigured.errors)"
    $config = [IO.File]::ReadAllText($configPath) | ConvertFrom-Json
    $provider = $config.providers.PSObject.Properties[$responsesProvider].Value
    Assert-Equal 'gemini-added gpt-plan' ((@($provider.models.PSObject.Properties.Name) | Sort-Object) -join ' ') 'Replaced same-origin catalog'
    Assert-Equal 'Custom Plan Name' $provider.models.'gpt-plan'.name 'Retained model name'
    Assert-Equal 1000 $provider.models.'gpt-plan'.limit.context 'Retained model metadata'
    Assert-Equal $responsesProvider $config.agents.plan.model.providerID 'Plan model provider'
    Assert-Equal 'gpt-plan' $config.agents.plan.model.model 'Plan model after update'
    Assert-Equal $responsesProvider $config.agents.build.model.providerID 'Build model provider'
    Assert-Equal 'gemini-added' $config.agents.build.model.model 'Build model after update'
    $provider = $config.providers.PSObject.Properties[$anthropicProvider].Value
    Assert-Equal 0 @($provider.models.PSObject.Properties).Count 'Empty protocol catalog'
    Assert-Equal 4590 $provider.settings.timeout 'Existing setting for empty protocol'
    Assert-Equal "{file:$secretPath}" $provider.settings.apiKey 'Shared API Key reference for empty protocol'
    Assert-Equal 'test-rotated-key' ([IO.File]::ReadAllText($secretPath)) 'Updated same-origin API Key'
    $provider = $config.providers.PSObject.Properties[$otherProvider].Value
    Assert-Equal 'gemini-extra gpt-extra gpt-plan' ((@($provider.models.PSObject.Properties.Name) | Sort-Object) -join ' ') 'Other origin catalog'
    Assert-Equal 'test-other-origin-key' ([IO.File]::ReadAllText($otherSecretPath)) 'Other origin API Key'

    if ($env:OPENCODE_TEST_CLI) {
        $cliModels = (& $env:OPENCODE_TEST_CLI models --standalone | Out-String)
        Assert-Equal 0 $LASTEXITCODE 'Updated OpenCode models exit status'
        Assert-Equal $true $cliModels.Contains("$responsesProvider/gemini-added") 'New model in OpenCode'
        Assert-Equal $false $cliModels.Contains("$responsesProvider/gpt-extra") 'Removed model in OpenCode'
        Assert-Equal $false $cliModels.Contains("$anthropicProvider/claude-build") 'Empty protocol in OpenCode'
        Assert-Equal $true $cliModels.Contains("$otherProvider/gpt-extra") 'Other origin in OpenCode'
    }
    Write-Output "OpenCode configurator regression tests passed on PowerShell $($PSVersionTable.PSVersion)."
} finally {
    $listener.Stop()
    foreach ($name in $originalEnv.Keys) {
        [Environment]::SetEnvironmentVariable($name, $originalEnv[$name], 'Process')
    }
    [IO.Directory]::Delete($prefix, $true)
}
