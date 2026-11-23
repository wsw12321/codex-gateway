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

function Get-ProviderPackage([object] $Model, [string] $Mode) {
    if ($null -eq $Model -or $Model.id -isnot [string] -or $Model.id -notmatch '^[^#\s\x00-\x1f\x7f]+$') {
        throw "安装脚本缺少有效的 $Mode 模型。请从使用指南重新下载脚本。"
    }
    switch -CaseSensitive ($Model.group) {
        'GPT'    { return '@opencode/ai/providers/openai-compatible/responses' }
        'Gemini' { return '@opencode/ai/providers/openai-compatible/responses' }
        'Claude' { return '@opencode/ai/providers/anthropic' }
        default  { throw "安装脚本的 $Mode 模型组无效。请从使用指南重新下载脚本。" }
    }
}

function Get-GatewayModels([string] $Origin, [string] $Key) {
    $tls = [Net.ServicePointManager]::SecurityProtocol
    try {
        [Net.ServicePointManager]::SecurityProtocol = $tls -bor [Net.SecurityProtocolType]::Tls12
        $request = [Net.WebRequest]::Create("$Origin/v1/models")
        $request.AllowAutoRedirect = $false
        $request.Timeout = 15000
        $request.ReadWriteTimeout = 15000
        $request.Headers['Authorization'] = "Bearer $Key"
        $response = $request.GetResponse()
        try {
            if ([int] $response.StatusCode -ne 200) {
                throw '网关模型列表请求未成功，请检查当前网关地址。'
            }
            $reader = New-Object IO.StreamReader($response.GetResponseStream(), $utf8)
            try {
                try {
                    $result = ConvertFrom-Json -InputObject $reader.ReadToEnd()
                } catch {
                    throw '网关返回的模型列表格式无效。'
                }
            } finally {
                $reader.Dispose()
            }
        } finally {
            $response.Dispose()
        }
    } catch [Net.WebException] {
        $status = $_.Exception.Response
        if ($null -ne $status) {
            try {
                throw "网关模型列表请求失败（HTTP $([int] $status.StatusCode)）。请检查 API Key 及网关地址。"
            } finally {
                $status.Dispose()
            }
        }
        throw '无法连接网关的模型列表。请检查网络后重试。'
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $tls
    }
    if ($result.data -isnot [Array]) {
        throw '网关返回的模型列表格式无效。'
    }
    return $result.data
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
    $origin = Get-SetupOrigin $setup.origin
    $planPackage = Get-ProviderPackage $setup.plan 'Plan'
    $buildPackage = Get-ProviderPackage $setup.build 'Build'
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

        $modelIds = @(Get-GatewayModels $origin $key | ForEach-Object { $_.id })
        $modes = @(
            @{ name = 'plan'; id = $setup.plan.id; package = $planPackage }
            @{ name = 'build'; id = $setup.build.id; package = $buildPackage }
        )
        foreach ($mode in $modes) {
            if ($modelIds -cnotcontains $mode.id) {
                throw "当前 API Key 的模型列表中没有 $($mode.name) 所选模型 $($mode.id)。"
            }
            $protocol = 'responses'
            if ($mode.package -eq '@opencode/ai/providers/anthropic') {
                $protocol = 'anthropic'
            }
            $providerID = "codex-gateway-$originHash-$protocol"
            $provider = Get-ConfigObject $providers $providerID
            Set-ConfigProperty $provider 'name' "Codex Gateway ($protocol)"
            Set-ConfigProperty $provider 'package' $mode.package
            $settings = Get-ConfigObject $provider 'settings'
            Set-ConfigProperty $settings 'baseURL' "$origin/v1"
            Set-ConfigProperty $settings 'apiKey' "{file:$secretPath}"
            $models = Get-ConfigObject $provider 'models'
            $model = Get-ConfigObject $models $mode.id
            Set-ConfigProperty $model 'name' $mode.id
            $agent = Get-ConfigObject $agents $mode.name
            Set-ConfigProperty $agent 'model' ([ordered] @{ providerID = $providerID; model = $mode.id })
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
