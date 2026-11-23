$ErrorActionPreference = 'Stop'
$setupPayload = '__OPENCODE_SETUP_PAYLOAD__'
$utf8 = New-Object Text.UTF8Encoding($false, $true)

function Get-SetupOrigin([string] $Value) {
    $uri = $null
    if (-not [Uri]::TryCreate($Value, [UriKind]::Absolute, [ref] $uri) -or
        ($uri.Scheme -ne 'https' -and ($uri.Scheme -ne 'http' -or -not $uri.IsLoopback)) -or
        $uri.UserInfo -or $uri.AbsolutePath -ne '/' -or $uri.Query -or $uri.Fragment) {
        throw '网关地址必须是 HTTPS 源地址，或本机 HTTP 源地址。请从使用指南重新下载脚本。'
    }
    return $uri.GetLeftPart([UriPartial]::Authority)
}

function Get-EncodingHash([string] $Text) {
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        return [BitConverter]::ToString($sha256.ComputeHash($utf8.GetBytes($Text))).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha256.Dispose()
    }
}

function Get-ModelProtocol([object] $Model) {
    if ($Model -isnot [pscustomobject] -or $Model.id -isnot [string] -or
        $Model.id -notmatch '^[^#{}\s\x00-\x1f\x7f]+$' -or $Model.group -isnot [string]) {
        throw '安装脚本包含无效的模型条目。请从使用指南重新下载脚本。'
    }
    switch -CaseSensitive ($Model.group) {
        'GPT'    { return 'responses' }
        'Gemini' { return 'responses' }
        'Claude' { return 'anthropic' }
        default  { throw '安装脚本包含无效的模型组。请从使用指南重新下载脚本。' }
    }
}

function Get-ConfigPath {
    if ($env:OPENCODE_CONFIG_CONTENT) {
        throw '检测到 OPENCODE_CONFIG_CONTENT。请先清除该配置覆盖变量，再运行脚本。'
    }
    if ($env:OPENCODE_CONFIG) {
        return $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($env:OPENCODE_CONFIG)
    }
    if ($env:OPENCODE_CONFIG_DIR) {
        $directory = $env:OPENCODE_CONFIG_DIR
    } elseif ($env:XDG_CONFIG_HOME) {
        $directory = Join-Path $env:XDG_CONFIG_HOME 'opencode'
    } else {
        $directory = Join-Path $HOME '.config\opencode'
    }
    $directory = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($directory)
    $jsonc = Join-Path $directory 'opencode.jsonc'
    $json = Join-Path $directory 'opencode.json'
    if (-not [IO.File]::Exists($jsonc) -and [IO.File]::Exists($json)) {
        return $json
    }
    return $jsonc
}

function Read-OpenCodeConfig([string] $Path) {
    if (-not [IO.File]::Exists($Path)) {
        return [pscustomobject] @{}
    }
    $content = [IO.File]::ReadAllText($Path, $utf8)
    # Match complete JSON strings before looking for JSONC comments or trailing commas.
    $content = [regex]::Replace($content, '"(?:\\.|[^"\\])*"|(?s:/\*.*?\*/)|//[^\r\n]*', {
        param($match)
        if ($match.Value.StartsWith('"')) { return $match.Value }
        return ' '
    })
    $content = [regex]::Replace($content, '"(?:\\.|[^"\\])*"|,(?=\s*[\]}])', {
        param($match)
        if ($match.Value.StartsWith('"')) { return $match.Value }
        return ''
    })
    if (-not $content.TrimStart().StartsWith('{')) {
        throw "现有 OpenCode 配置必须是 JSON 对象。请检查 $Path。"
    }
    $parseOptions = @{ InputObject = $content }
    if ((Get-Command ConvertFrom-Json).Parameters.ContainsKey('DateKind')) {
        $parseOptions.DateKind = 'String'
    }
    try {
        $config = ConvertFrom-Json @parseOptions
    } catch {
        throw "现有 OpenCode 配置不是有效的 JSON 或 JSONC。请检查 $Path。"
    }
    if ($config -isnot [pscustomobject]) {
        throw "无法读取 OpenCode 配置 $Path。"
    }
    return $config
}

function Set-ConfigProperty([object] $Object, [string] $Name, [object] $Value) {
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        $Object.PSObject.Properties.Add((New-Object Management.Automation.PSNoteProperty($Name, $Value)))
    } else {
        $property.Value = $Value
    }
}

function Get-ConfigObject([object] $Object, [string] $Name) {
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        $value = [pscustomobject] @{}
        Set-ConfigProperty $Object $Name $value
        return $value
    }
    if ($property.Value -isnot [pscustomobject]) {
        throw "现有 OpenCode 配置的 $Name 必须是对象。"
    }
    return $property.Value
}

function Write-AtomicFile([string] $Path, [string] $Content, [string] $Backup) {
    $directory = [IO.Path]::GetDirectoryName($Path)
    [void] [IO.Directory]::CreateDirectory($directory)
    $staged = Join-Path $directory ('.opencode-setup-' + [Guid]::NewGuid().ToString('N') + '.tmp')
    try {
        [IO.File]::WriteAllText($staged, $Content, $utf8)
        if ([IO.File]::Exists($Path)) {
            [IO.File]::Replace($staged, $Path, $Backup)
        } else {
            [IO.File]::Move($staged, $Path)
        }
    } finally {
        if ([IO.File]::Exists($staged)) {
            [IO.File]::Delete($staged)
        }
    }
}

try {
    try {
        $setup = ConvertFrom-Json -InputObject $utf8.GetString([Convert]::FromBase64String($setupPayload))
    } catch {
        throw '安装脚本缺少有效的配置信息。请从使用指南重新下载脚本。'
    }
    if ($setup -isnot [pscustomobject] -or $setup.origin -isnot [string] -or
        $setup.models -isnot [Array] -or $setup.models.Length -eq 0) {
        throw '安装脚本缺少有效的模型目录。请从使用指南重新下载脚本。'
    }
    $origin = Get-SetupOrigin $setup.origin
    $packages = [ordered] @{
        responses = '@opencode/ai/providers/openai-compatible/responses'
        anthropic = '@opencode/ai/providers/anthropic'
    }
    $catalog = New-Object 'Collections.Generic.Dictionary[string,string]' ([StringComparer]::Ordinal)
    $modelsByProtocol = @{}
    foreach ($protocol in $packages.Keys) {
        $modelsByProtocol[$protocol] = New-Object Collections.Specialized.OrderedDictionary([StringComparer]::OrdinalIgnoreCase)
    }
    foreach ($model in $setup.models) {
        $protocol = Get-ModelProtocol $model
        if ($catalog.ContainsKey($model.id)) {
            throw '安装脚本包含重复的模型 ID。请从使用指南重新下载脚本。'
        }
        # Windows PowerShell 5.1 cannot read JSON object keys that differ only in case.
        if ($modelsByProtocol[$protocol].Contains($model.id)) {
            throw '同一接口的模型 ID 不能仅靠大小写区分。请联系站点管理员核对模型目录。'
        }
        $catalog.Add($model.id, $model.group)
        $modelsByProtocol[$protocol].Add($model.id, @{ name = $model.id })
    }
    $modes = [ordered] @{ plan = $setup.plan; build = $setup.build }
    $modeProtocols = @{}
    foreach ($mode in $modes.Keys) {
        $model = $modes[$mode]
        $modeProtocols[$mode] = Get-ModelProtocol $model
        if (-not $catalog.ContainsKey($model.id) -or $catalog[$model.id] -cne $model.group) {
            throw "安装脚本的 $mode 模型不在所附目录中，或模型组不匹配。请从使用指南重新下载脚本。"
        }
    }
    $configPath = Get-ConfigPath
    $originHash = (Get-EncodingHash $origin).Substring(0, 16)
    $configHash = (Get-EncodingHash $configPath.ToUpperInvariant()).Substring(0, 16)
    $secretDirectory = Join-Path $env:LOCALAPPDATA 'codex-gateway\opencode'
    $secretPath = Join-Path $secretDirectory "$originHash-$configHash.key"
    if ($secretPath.Contains('}')) {
        throw '本机密钥目录含有 OpenCode 无法识别的字符。'
    }
    $mutex = New-Object Threading.Mutex($false, "Local\codex-gateway-opencode-$configHash")
    $locked = $false
    try {
        try {
            $locked = $mutex.WaitOne(0)
        } catch [Threading.AbandonedMutexException] {
            $locked = $true
        }
        if (-not $locked) {
            throw '此 OpenCode 配置正在由另一个安装脚本更新。请稍后重试。'
        }

        $config = Read-OpenCodeConfig $configPath
        $providers = Get-ConfigObject $config 'providers'
        $agents = Get-ConfigObject $config 'agents'

        Write-Host "网关地址 $origin"
        $secureKey = Read-Host '请输入网关 API Key（输入不会显示）' -AsSecureString
        $keyBuffer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
        try {
            $key = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($keyBuffer)
        } finally {
            [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($keyBuffer)
            $secureKey.Dispose()
        }
        if ($key -notmatch '^[!-~]+$') {
            throw 'API Key 不能为空，也不能包含空格或换行。'
        }

        foreach ($protocol in $packages.Keys) {
            $providerID = "codex-gateway-$originHash-$protocol"
            $models = $modelsByProtocol[$protocol]
            if ($models.Count -eq 0 -and $null -eq $providers.PSObject.Properties[$providerID]) {
                continue
            }
            $provider = Get-ConfigObject $providers $providerID
            Set-ConfigProperty $provider 'name' "Codex Gateway ($protocol)"
            Set-ConfigProperty $provider 'package' $packages[$protocol]
            $settings = Get-ConfigObject $provider 'settings'
            Set-ConfigProperty $settings 'baseURL' "$origin/v1"
            Set-ConfigProperty $settings 'apiKey' "{file:$secretPath}"
            $existingModels = $provider.PSObject.Properties['models']
            if ($null -ne $existingModels -and $existingModels.Value -is [pscustomobject]) {
                foreach ($id in @($models.Keys)) {
                    $existing = $existingModels.Value.PSObject.Properties[$id]
                    if ($null -ne $existing -and $existing.Name -ceq $id -and $existing.Value -is [pscustomobject]) {
                        $models[[string] $id] = $existing.Value
                    }
                }
            }
            Set-ConfigProperty $provider 'models' $models
        }
        foreach ($mode in $modes.Keys) {
            $providerID = "codex-gateway-$originHash-$($modeProtocols[$mode])"
            $agent = Get-ConfigObject $agents $mode
            Set-ConfigProperty $agent 'model' ([ordered] @{ providerID = $providerID; model = $modes[$mode].id })
        }

        $updated = (ConvertTo-Json -InputObject $config -Depth 100 -WarningAction Stop) + [Environment]::NewLine
        $suffix = [Guid]::NewGuid().ToString('N')
        $configBackup = "$configPath.bak-$suffix"
        $secretBackup = "$secretPath.bak-$suffix"
        $savedSecret = [IO.File]::Exists($secretPath)
        Write-AtomicFile $secretPath $key $secretBackup
        try {
            Write-AtomicFile $configPath $updated $configBackup
        } catch {
            if ($savedSecret) {
                $discardPath = "$secretPath.rollback-$suffix"
                [IO.File]::Replace($secretBackup, $secretPath, $discardPath)
                [IO.File]::Delete($discardPath)
            } else {
                [IO.File]::Delete($secretPath)
            }
            throw
        }
        if ([IO.File]::Exists($secretBackup)) {
            [IO.File]::Delete($secretBackup)
        }
    } finally {
        $key = $null
        if ($locked) {
            $mutex.ReleaseMutex()
        }
        $mutex.Dispose()
    }
    Write-Host "已配置 OpenCode V2，Plan 使用 $($setup.plan.id)，Build 使用 $($setup.build.id)。"
    Write-Host "配置文件 $configPath"
    if ([IO.File]::Exists($configBackup)) {
        Write-Host "原配置备份 $configBackup"
    }
    Write-Host '请重新打开 OpenCode。已有会话仍使用会话中选择的模型。'
} catch {
    Write-Error "OpenCode 配置失败。$($_.Exception.Message)" -ErrorAction Continue
    exit 1
}
