@echo off
setlocal EnableExtensions
set "BATCH_PATH=%~f0"

fltmc >nul 2>&1
if errorlevel 1 (
    echo Requesting administrator permission...
    powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "try { $arguments = @('/d', '/c', ([char]34 + $env:BATCH_PATH + [char]34)); $process = Start-Process -FilePath $env:ComSpec -ArgumentList $arguments -Verb RunAs -Wait -PassThru; exit $process.ExitCode } catch { Write-Error $_; exit 1 }"
    exit /b
)

powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "$batchPath = $env:BATCH_PATH; $lines = [IO.File]::ReadAllLines($batchPath); $marker = [Array]::IndexOf($lines, '# POWERSHELL_START'); if ($marker -lt 0) { throw 'Embedded PowerShell code marker not found.' }; $code = $lines[($marker + 1)..($lines.Length - 1)] -join [Environment]::NewLine; try { & ([scriptblock]::Create($code)); exit 0 } catch { Write-Error $_; exit 1 }"
set "exitCode=%ERRORLEVEL%"

echo.
if not "%exitCode%"=="0" (
    echo The script failed. Exit code: %exitCode%
) else (
    echo Finished successfully.
)
pause
exit /b %exitCode%

# POWERSHELL_START
[CmdletBinding()]
param(
    [ValidateRange(1, 120)]
    [int]$TimeoutSec = 15,

    [int]$MeasuredAttempts = 3
)

$ErrorActionPreference = 'Stop'

$domains = @(
    'https://mofa.love.gd',
    'https://key66.vip',
    'https://mofayaoshipu.cc.cd',
    'https://key66.cc.cd'
)
$hostName = 'mofa.love.gd'
$hostIp = '15.204.82.11'

if ($MeasuredAttempts -ne 3) {
    throw 'MeasuredAttempts must remain exactly 3.'
}

if ([string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
    throw 'USERPROFILE is unavailable.'
}

if ([string]::IsNullOrWhiteSpace($env:SystemRoot)) {
    throw 'SystemRoot is unavailable.'
}

# Force TLS 1.2 for Windows PowerShell 5.1 while keeping certificate checks enabled.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Test-ByteArraysEqual {
    param(
        [byte[]]$Left,
        [byte[]]$Right
    )

    if ($null -eq $Left -or $null -eq $Right -or $Left.Length -ne $Right.Length) {
        return $false
    }

    for ($index = 0; $index -lt $Left.Length; $index++) {
        if ($Left[$index] -ne $Right[$index]) {
            return $false
        }
    }

    return $true
}

function Get-Sha256Hex {
    param([byte[]]$Bytes)

    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        return [BitConverter]::ToString($sha256.ComputeHash($Bytes)).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha256.Dispose()
    }
}

function ConvertTo-SnapshotBytes {
    param(
        [string]$Text,
        [Text.Encoding]$Encoding,
        [byte[]]$Preamble
    )

    [byte[]]$body = $Encoding.GetBytes($Text)
    [byte[]]$combined = @($Preamble) + @($body)
    return $combined
}

function Read-TextSnapshot {
    param([string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Required file does not exist: $Path"
    }

    [byte[]]$bytes = [IO.File]::ReadAllBytes($Path)
    [Text.Encoding]$encoding = $null
    [byte[]]$preamble = @()
    $offset = 0

    if ($bytes.Length -ge 4 -and $bytes[0] -eq 0x00 -and $bytes[1] -eq 0x00 -and $bytes[2] -eq 0xFE -and $bytes[3] -eq 0xFF) {
        $encoding = New-Object Text.UTF32Encoding($true, $true, $true)
        $preamble = $bytes[0..3]
        $offset = 4
    } elseif ($bytes.Length -ge 4 -and $bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE -and $bytes[2] -eq 0x00 -and $bytes[3] -eq 0x00) {
        $encoding = New-Object Text.UTF32Encoding($false, $true, $true)
        $preamble = $bytes[0..3]
        $offset = 4
    } elseif ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) {
        $encoding = New-Object Text.UTF8Encoding($true, $true)
        $preamble = $bytes[0..2]
        $offset = 3
    } elseif ($bytes.Length -ge 2 -and $bytes[0] -eq 0xFE -and $bytes[1] -eq 0xFF) {
        $encoding = New-Object Text.UnicodeEncoding($true, $true, $true)
        $preamble = $bytes[0..1]
        $offset = 2
    } elseif ($bytes.Length -ge 2 -and $bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE) {
        $encoding = New-Object Text.UnicodeEncoding($false, $true, $true)
        $preamble = $bytes[0..1]
        $offset = 2
    } else {
        $strictUtf8 = New-Object Text.UTF8Encoding($false, $true)
        try {
            [void]$strictUtf8.GetString($bytes)
            $encoding = $strictUtf8
        } catch {
            $encoding = [Text.Encoding]::Default
        }
    }

    $bodyLength = $bytes.Length - $offset
    $text = if ($bodyLength -gt 0) {
        $encoding.GetString($bytes, $offset, $bodyLength)
    } else {
        ''
    }

    [byte[]]$roundTrip = ConvertTo-SnapshotBytes -Text $text -Encoding $encoding -Preamble $preamble
    if (-not (Test-ByteArraysEqual -Left $bytes -Right $roundTrip)) {
        throw "The file encoding cannot be preserved safely: $Path"
    }

    return [PSCustomObject]@{
        Path     = $Path
        Bytes    = $bytes
        Text     = $text
        Encoding = $encoding
        Preamble = $preamble
        Hash     = Get-Sha256Hex -Bytes $bytes
    }
}

function Write-VerifiedBackup {
    param(
        [string]$Path,
        [byte[]]$Bytes
    )

    if (Test-Path -LiteralPath $Path) {
        throw "Refusing to overwrite an existing backup: $Path"
    }

    [IO.File]::WriteAllBytes($Path, $Bytes)
    [byte[]]$written = [IO.File]::ReadAllBytes($Path)
    if (-not (Test-ByteArraysEqual -Left $Bytes -Right $written)) {
        throw "Backup verification failed: $Path"
    }
}

function Write-AtomicBytes {
    param(
        [string]$Path,
        [byte[]]$Bytes,
        [string]$RunId
    )

    $temporaryPath = "$Path.codex-speed.$RunId.tmp"
    if (Test-Path -LiteralPath $temporaryPath) {
        throw "Temporary path already exists: $temporaryPath"
    }

    try {
        [IO.File]::WriteAllBytes($temporaryPath, $Bytes)
        [byte[]]$temporaryBytes = [IO.File]::ReadAllBytes($temporaryPath)
        if (-not (Test-ByteArraysEqual -Left $Bytes -Right $temporaryBytes)) {
            throw "Temporary file verification failed: $temporaryPath"
        }

        [IO.File]::Replace($temporaryPath, $Path, $null)
    } finally {
        if (Test-Path -LiteralPath $temporaryPath) {
            Remove-Item -LiteralPath $temporaryPath -Force
        }
    }
}

function Get-LineRecords {
    param([string]$Text)

    $matches = [regex]::Matches($Text, '(?m)^[^\r\n]*(?:\r\n|\n|\r|$)')
    foreach ($match in $matches) {
        if ($match.Length -eq 0) {
            continue
        }

        [PSCustomObject]@{
            Start = $match.Index
            Length = $match.Length
            Text = $match.Value.TrimEnd("`r", "`n")
        }
    }
}

function Get-CodeBeforeComment {
    param([string]$Line)

    $quote = [char]0
    $escaped = $false

    for ($index = 0; $index -lt $Line.Length; $index++) {
        $character = $Line[$index]

        if ($quote -ne [char]0) {
            if ($quote -eq '"' -and $character -eq '\' -and -not $escaped) {
                $escaped = $true
                continue
            }

            if ($character -eq $quote -and -not $escaped) {
                $quote = [char]0
            }

            $escaped = $false
            continue
        }

        if ($character -eq '"' -or $character -eq "'") {
            $quote = $character
            continue
        }

        if ($character -eq '#') {
            return $Line.Substring(0, $index)
        }
    }

    if ($quote -ne [char]0) {
        throw 'Unsupported unterminated TOML string.'
    }

    return $Line
}

function Read-SimpleTomlString {
    param(
        [string]$SerializedValue,
        [string]$SettingName
    )

    $trimmed = $SerializedValue.Trim()
    if ($trimmed.Length -lt 2) {
        throw "Unsupported $SettingName value."
    }

    $quote = $trimmed[0]
    if (($quote -ne '"' -and $quote -ne "'") -or $trimmed[$trimmed.Length - 1] -ne $quote) {
        throw "$SettingName must be a simple quoted TOML string."
    }

    $inner = $trimmed.Substring(1, $trimmed.Length - 2)
    if ($inner.Contains('\') -or $inner.Contains([string]$quote)) {
        throw "$SettingName uses unsupported escapes or quotes."
    }

    return $inner
}

function Get-CodexConfigTarget {
    param([string]$Text)

    if ($Text.Contains('"""') -or $Text.Contains("'''")) {
        throw 'Multiline TOML strings are not supported safely.'
    }

    $lines = @(Get-LineRecords -Text $Text)
    if ($lines.Count -eq 0) {
        throw 'Codex config.toml is empty.'
    }

    $providerMatches = @()
    $insideTable = $false

    for ($index = 0; $index -lt $lines.Count; $index++) {
        $code = (Get-CodeBeforeComment -Line $lines[$index].Text).Trim()
        if ([string]::IsNullOrWhiteSpace($code)) {
            continue
        }

        if ($code -match '^\[.*\]$') {
            $insideTable = $true
            continue
        }

        if (-not $insideTable -and $code -match '^model_provider\s*=\s*(?<value>.+)$') {
            $providerMatches += [PSCustomObject]@{
                Value = Read-SimpleTomlString -SerializedValue $Matches['value'] -SettingName 'model_provider'
                LineIndex = $index
            }
        }
    }

    if ($providerMatches.Count -ne 1) {
        throw "Expected exactly one top-level model_provider, found $($providerMatches.Count)."
    }

    $provider = $providerMatches[0].Value
    if ($provider -notmatch '^[A-Za-z0-9_-]+$') {
        throw 'The selected model_provider name is not supported safely.'
    }

    $targetHeaders = @()
    $bareHeader = "[model_providers.$provider]"
    $doubleQuotedHeader = '[model_providers."' + $provider + '"]'
    $singleQuotedHeader = "[model_providers.'$provider']"

    for ($index = 0; $index -lt $lines.Count; $index++) {
        $code = (Get-CodeBeforeComment -Line $lines[$index].Text).Trim()
        if ($code -eq $bareHeader -or $code -eq $doubleQuotedHeader -or $code -eq $singleQuotedHeader) {
            $targetHeaders += $index
        }
    }

    if ($targetHeaders.Count -ne 1) {
        throw "Expected exactly one table for the selected model_provider, found $($targetHeaders.Count)."
    }

    $sectionStart = $targetHeaders[0] + 1
    $sectionEnd = $lines.Count
    for ($index = $sectionStart; $index -lt $lines.Count; $index++) {
        $code = (Get-CodeBeforeComment -Line $lines[$index].Text).Trim()
        if ($code -match '^\[.*\]$') {
            $sectionEnd = $index
            break
        }
    }

    $baseUrlMatches = @()
    $baseUrlPattern = '^(?<prefix>[ \t]*base_url[ \t]*=[ \t]*)(?<quote>["''])(?<url>[^"'']+)\k<quote>(?<suffix>[ \t]*(?:#.*)?)$'
    for ($index = $sectionStart; $index -lt $sectionEnd; $index++) {
        $match = [regex]::Match($lines[$index].Text, $baseUrlPattern)
        if ($match.Success) {
            $baseUrlMatches += [PSCustomObject]@{
                Url = $match.Groups['url'].Value
                UrlStart = $lines[$index].Start + $match.Groups['url'].Index
                UrlLength = $match.Groups['url'].Length
                LineIndex = $index
            }
        }
    }

    if ($baseUrlMatches.Count -ne 1) {
        throw "Expected exactly one base_url in the selected provider table, found $($baseUrlMatches.Count)."
    }

    $baseUrl = $baseUrlMatches[0].Url
    $uri = $null
    if (-not [Uri]::TryCreate($baseUrl, [UriKind]::Absolute, [ref]$uri)) {
        throw 'The selected provider base_url is not a valid absolute URL.'
    }

    if ($uri.Scheme -ne 'http' -and $uri.Scheme -ne 'https') {
        throw 'The selected provider base_url must use HTTP or HTTPS.'
    }

    return [PSCustomObject]@{
        Provider = $provider
        Url = $baseUrl
        UrlStart = $baseUrlMatches[0].UrlStart
        UrlLength = $baseUrlMatches[0].UrlLength
    }
}

function Set-UrlHostOnly {
    param(
        [string]$Url,
        [string]$NewHost
    )

    $urlPattern = '^(?<scheme>https?://)(?<userinfo>[^/@?#]+@)?(?<host>\[[^\]]+\]|[^:/?#]+)(?<rest>.*)$'
    $match = [regex]::Match($Url, $urlPattern, [Text.RegularExpressions.RegexOptions]::IgnoreCase)
    if (-not $match.Success) {
        throw 'The provider base_url structure is not supported safely.'
    }

    $updatedUrl = $match.Groups['scheme'].Value + $match.Groups['userinfo'].Value + $NewHost + $match.Groups['rest'].Value
    $originalUri = [Uri]$Url
    $updatedUri = [Uri]$updatedUrl

    if ($originalUri.Scheme -ne $updatedUri.Scheme -or
        $originalUri.Port -ne $updatedUri.Port -or
        $originalUri.UserInfo -ne $updatedUri.UserInfo -or
        $originalUri.PathAndQuery -ne $updatedUri.PathAndQuery -or
        $originalUri.Fragment -ne $updatedUri.Fragment -or
        $updatedUri.Host -ne $NewHost) {
        throw 'Changing the host would alter another base_url component.'
    }

    return $updatedUrl
}

function Update-CodexConfigHost {
    param(
        [string]$Text,
        [string]$NewHost
    )

    $target = Get-CodexConfigTarget -Text $Text
    $updatedUrl = Set-UrlHostOnly -Url $target.Url -NewHost $NewHost
    $updatedText = $Text.Substring(0, $target.UrlStart) + $updatedUrl + $Text.Substring($target.UrlStart + $target.UrlLength)
    $updatedTarget = Get-CodexConfigTarget -Text $updatedText

    if ($updatedTarget.Provider -ne $target.Provider -or $updatedTarget.Url -ne $updatedUrl) {
        throw 'The updated Codex config failed structural validation.'
    }

    return [PSCustomObject]@{
        Text = $updatedText
        Provider = $target.Provider
        PreviousHost = ([Uri]$target.Url).Host
        UpdatedHost = ([Uri]$updatedUrl).Host
    }
}

function Get-UpdatedHostsText {
    param(
        [string]$Text,
        [string]$Name,
        [string]$Address
    )

    $pattern = '(?im)^(?<prefix>[ \t]*)(?!#)(?<ip>\S+)(?<separator>[ \t]+)(?<aliases>[^#\r\n]*?(?<![A-Za-z0-9.-])' + [regex]::Escape($Name) + '(?![A-Za-z0-9.-])[^#\r\n]*)(?<comment>[ \t]*(?:#.*)?$)'
    $matches = [regex]::Matches($Text, $pattern)

    if ($matches.Count -gt 0) {
        if ($matches.Count -ne 1) {
            throw "Multiple active hosts mappings exist for $Name; resolve them manually first."
        }

        $aliases = @($matches[0].Groups['aliases'].Value -split '[ \t]+' | Where-Object { $_ })
        if ($aliases.Count -ne 1 -or -not [string]::Equals($aliases[0], $Name, [StringComparison]::OrdinalIgnoreCase)) {
            throw "The hosts mapping for $Name shares a line with other aliases; resolve it manually first."
        }

        $updated = $Text
        for ($index = $matches.Count - 1; $index -ge 0; $index--) {
            $match = $matches[$index]
            $replacement = $match.Groups['prefix'].Value + $Address + $match.Groups['separator'].Value + $match.Groups['aliases'].Value + $match.Groups['comment'].Value
            $updated = $updated.Substring(0, $match.Index) + $replacement + $updated.Substring($match.Index + $match.Length)
        }
        return $updated
    }

    $lineEnding = if ($Text.Contains("`r`n")) {
        "`r`n"
    } elseif ($Text.Contains("`n")) {
        "`n"
    } elseif ($Text.Contains("`r")) {
        "`r"
    } else {
        [Environment]::NewLine
    }

    if ([string]::IsNullOrEmpty($Text)) {
        return $Address + ' ' + $Name + $lineEnding
    }

    return $Text.TrimEnd("`r", "`n") + $lineEnding + $Address + ' ' + $Name + $lineEnding
}

function Invoke-HealthSample {
    param(
        [string]$Origin,
        [int]$Timeout
    )

    $healthUrl = $Origin.TrimEnd('/') + '/health'
    $expectedHost = ([Uri]$Origin).Host
    $stopwatch = [Diagnostics.Stopwatch]::StartNew()
    try {
        $response = Invoke-WebRequest -Uri $healthUrl -Method Get -UseBasicParsing -MaximumRedirection 3 -TimeoutSec $Timeout -Headers @{ 'Cache-Control' = 'no-cache' }
    } finally {
        $stopwatch.Stop()
    }

    if ($response.StatusCode -ne 200) {
        throw "HTTP $($response.StatusCode)"
    }

    if ($response.BaseResponse.ResponseUri.Host -ne $expectedHost) {
        throw 'Health check redirected to another host.'
    }

    if ($response.Content -cnotmatch '^\s*\{\s*"status"\s*:\s*"ok"\s*\}\s*$') {
        throw 'Health check response was not exactly status=ok.'
    }

    try {
        $payload = $response.Content | ConvertFrom-Json
    } catch {
        throw 'Health check did not return valid JSON.'
    }

    $properties = @($payload.PSObject.Properties.Name)
    if ($properties.Count -ne 1 -or $properties[0] -ne 'status' -or $payload.status -ne 'ok') {
        throw 'Health check response was not exactly status=ok.'
    }

    return $stopwatch.Elapsed.TotalMilliseconds
}

function Measure-Domains {
    param(
        [string[]]$Origins,
        [int]$Timeout,
        [int]$Attempts
    )

    foreach ($origin in $Origins) {
        $times = @()
        $errors = @()
        $warmup = 'ok'

        try {
            [void](Invoke-HealthSample -Origin $origin -Timeout $Timeout)
        } catch {
            $warmup = $_.Exception.Message
        }

        for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
            try {
                $times += [double](Invoke-HealthSample -Origin $origin -Timeout $Timeout)
            } catch {
                $errors += "attempt ${attempt}: $($_.Exception.Message)"
            }
        }

        $eligible = $times.Count -eq $Attempts
        $median = if ($eligible) {
            [math]::Round(($times | Sort-Object)[1], 2)
        } else {
            $null
        }

        [PSCustomObject]@{
            Origin = $origin
            Host = ([Uri]$origin).Host
            MedianMs = $median
            Passed = $times.Count
            Eligible = $eligible
            Warmup = $warmup
            Error = ($errors -join '; ')
        }
    }
}

function Restore-OwnedChange {
    param(
        [string]$Path,
        [string]$ExpectedHash,
        [byte[]]$OriginalBytes,
        [string]$RunId
    )

    [byte[]]$currentBytes = [IO.File]::ReadAllBytes($Path)
    $currentHash = Get-Sha256Hex -Bytes $currentBytes
    if ($currentHash -ne $ExpectedHash) {
        throw "Refusing automatic rollback because another process changed: $Path"
    }

    Write-AtomicBytes -Path $Path -Bytes $OriginalBytes -RunId ($RunId + '-rollback')
    [byte[]]$restored = [IO.File]::ReadAllBytes($Path)
    if (-not (Test-ByteArraysEqual -Left $OriginalBytes -Right $restored)) {
        throw "Rollback verification failed: $Path"
    }
}

$mutexCreated = $false
$mutex = New-Object Threading.Mutex($true, 'Global\Sub2API-CodexBaseUrlSelector', [ref]$mutexCreated)
if (-not $mutexCreated) {
    $mutex.Dispose()
    throw 'Another copy of this script is already running.'
}

try {
    $runId = (Get-Date).ToUniversalTime().ToString('yyyyMMdd-HHmmss-fff') + '-' + $PID
    $configDirectory = Join-Path $env:USERPROFILE '.codex'
    $configPath = Join-Path $configDirectory 'config.toml'
    $hostsPath = Join-Path $env:SystemRoot 'System32\drivers\etc\hosts'

    $configSnapshot = Read-TextSnapshot -Path $configPath
    [void](Get-CodexConfigTarget -Text $configSnapshot.Text)
    $hostsSnapshot = Read-TextSnapshot -Path $hostsPath

    $configBackupPath = "$configPath.codex-speed.$runId.bak"
    $hostsBackupPath = "$hostsPath.codex-speed.$runId.bak"
    Write-VerifiedBackup -Path $configBackupPath -Bytes $configSnapshot.Bytes
    Write-VerifiedBackup -Path $hostsBackupPath -Bytes $hostsSnapshot.Bytes

    $hostsTouched = $false
    $configTouched = $false
    $hostsAppliedHash = ''
    $configAppliedHash = ''

    try {
        $updatedHostsText = Get-UpdatedHostsText -Text $hostsSnapshot.Text -Name $hostName -Address $hostIp
        [byte[]]$updatedHostsBytes = ConvertTo-SnapshotBytes -Text $updatedHostsText -Encoding $hostsSnapshot.Encoding -Preamble $hostsSnapshot.Preamble
        if (-not (Test-ByteArraysEqual -Left $hostsSnapshot.Bytes -Right $updatedHostsBytes)) {
            [byte[]]$currentHostsBytes = [IO.File]::ReadAllBytes($hostsPath)
            if ((Get-Sha256Hex -Bytes $currentHostsBytes) -ne $hostsSnapshot.Hash) {
                throw 'The hosts file changed after preflight; no hosts update was attempted.'
            }

            Write-AtomicBytes -Path $hostsPath -Bytes $updatedHostsBytes -RunId $runId
            $hostsTouched = $true
            $hostsAppliedHash = Get-Sha256Hex -Bytes $updatedHostsBytes
        }

        ipconfig.exe /flushdns | Out-Host
        if ($LASTEXITCODE -ne 0) {
            throw 'DNS cache flush failed after updating hosts.'
        }

        Write-Host ("Hosts mapping active: {0} -> {1}" -f $hostName, $hostIp) -ForegroundColor Green
        Write-Host ''
        Write-Host 'Testing public health endpoints...' -ForegroundColor Cyan
        $results = @(Measure-Domains -Origins $domains -Timeout $TimeoutSec -Attempts $MeasuredAttempts)

        Write-Host ''
        Write-Host 'Test results:' -ForegroundColor Cyan
        foreach ($result in $results) {
            if ($result.Eligible) {
                Write-Host ("  {0} - {1} ms median (3/3 passed)" -f $result.Origin, $result.MedianMs)
            } else {
                $detail = if ($result.Error) { $result.Error } else { 'insufficient successful samples' }
                Write-Host ("  {0} - unavailable ({1}/3 passed): {2}" -f $result.Origin, $result.Passed, $detail) -ForegroundColor DarkYellow
            }
            if ($result.Warmup -ne 'ok') {
                Write-Host ("    warm-up: {0}" -f $result.Warmup) -ForegroundColor DarkYellow
            }
        }

        $best = $results |
            Where-Object { $_.Eligible } |
            Sort-Object MedianMs, Origin |
            Select-Object -First 1

        if ($null -eq $best) {
            throw 'All domains failed the required 3/3 health checks.'
        }

        [byte[]]$currentConfigBytes = [IO.File]::ReadAllBytes($configPath)
        if ((Get-Sha256Hex -Bytes $currentConfigBytes) -ne $configSnapshot.Hash) {
            throw 'Codex config.toml changed after preflight; no config update was attempted.'
        }

        $updatedConfig = Update-CodexConfigHost -Text $configSnapshot.Text -NewHost $best.Host
        [byte[]]$updatedConfigBytes = ConvertTo-SnapshotBytes -Text $updatedConfig.Text -Encoding $configSnapshot.Encoding -Preamble $configSnapshot.Preamble
        if (-not (Test-ByteArraysEqual -Left $configSnapshot.Bytes -Right $updatedConfigBytes)) {
            Write-AtomicBytes -Path $configPath -Bytes $updatedConfigBytes -RunId $runId
            $configTouched = $true
            $configAppliedHash = Get-Sha256Hex -Bytes $updatedConfigBytes
        }

        $verifiedConfig = Read-TextSnapshot -Path $configPath
        $verifiedTarget = Get-CodexConfigTarget -Text $verifiedConfig.Text
        $expectedConfigHash = if ($configTouched) { $configAppliedHash } else { $configSnapshot.Hash }
        if ($verifiedConfig.Hash -ne $expectedConfigHash -or
            $verifiedTarget.Provider -ne $updatedConfig.Provider -or
            ([Uri]$verifiedTarget.Url).Host -ne $best.Host) {
            throw 'Final Codex config verification failed.'
        }

        $expectedHostsHash = if ($hostsTouched) { $hostsAppliedHash } else { $hostsSnapshot.Hash }
        if ((Get-Sha256Hex -Bytes ([IO.File]::ReadAllBytes($hostsPath))) -ne $expectedHostsHash) {
            throw 'The hosts file changed before final verification.'
        }

        Write-Host ''
        Write-Host ("Selected: {0} ({1} ms median)" -f $best.Origin, $best.MedianMs) -ForegroundColor Green
        Write-Host ("Provider: {0} (selection unchanged)" -f $updatedConfig.Provider)
        Write-Host ("Updated host: {0}" -f $best.Host)
        Write-Host ("Config: {0}" -f $configPath)
        Write-Host ("Config backup: {0}" -f $configBackupPath)
        Write-Host ("Hosts: {0}" -f $hostsPath)
        Write-Host ("Hosts backup: {0}" -f $hostsBackupPath)
        Write-Host 'Keep both backups until Codex and network access have been verified.' -ForegroundColor Yellow
    } catch {
        $primaryError = $_.Exception.Message
        $rollbackErrors = @()

        if ($configTouched) {
            try {
                Restore-OwnedChange -Path $configPath -ExpectedHash $configAppliedHash -OriginalBytes $configSnapshot.Bytes -RunId $runId
            } catch {
                $rollbackErrors += $_.Exception.Message
            }
        }

        if ($hostsTouched) {
            try {
                Restore-OwnedChange -Path $hostsPath -ExpectedHash $hostsAppliedHash -OriginalBytes $hostsSnapshot.Bytes -RunId $runId
                ipconfig.exe /flushdns | Out-Host
                if ($LASTEXITCODE -ne 0) {
                    throw 'DNS cache flush failed during rollback.'
                }
            } catch {
                $rollbackErrors += $_.Exception.Message
            }
        }

        if ($rollbackErrors.Count -gt 0) {
            throw ($primaryError + ' Automatic rollback was incomplete: ' + ($rollbackErrors -join ' | ') + " Use backups: $configBackupPath and $hostsBackupPath")
        }

        throw ($primaryError + ' Changes made by this run were rolled back. Backups remain at: ' + $configBackupPath + ' and ' + $hostsBackupPath)
    }
} finally {
    if ($mutexCreated) {
        try {
            $mutex.ReleaseMutex()
        } catch {
            # The process is already exiting; disposal still releases the handle.
        }
    }
    $mutex.Dispose()
}
