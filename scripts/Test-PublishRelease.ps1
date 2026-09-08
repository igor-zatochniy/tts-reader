$ErrorActionPreference = 'Stop'
$script:commit = '1234567890123456789012345678901234567890'
$script:state = $null

# Fake gh ізолює тести від GitHub та відтворює частково виконані операції.
function gh {
    $a = @($args)
    $state = $global:ttsReleaseTestState
    $global:LASTEXITCODE = 0
    if ($a[0] -eq 'api') {
        return $state.RemoteCommit
    }
    switch ($a[1]) {
        'view' {
            if (!$state.Exists) { $global:LASTEXITCODE = 1; return }
            return @{ isDraft = $state.Draft } | ConvertTo-Json -Compress
        }
        'create' {
            if ($a -notcontains '--draft' -or $a -notcontains '--verify-tag') { throw 'Unsafe release creation.' }
            $state.Exists = $true
            $state.Draft = $true
        }
        'upload' {
            if (!$state.Draft -or $a -notcontains '--clobber') { throw 'Unsafe asset replacement.' }
            $state.Uploads++
            if ($state.Uploads -eq $state.FailUpload) {
                $global:LASTEXITCODE = 1
                return
            }
            $state.Assets[[IO.Path]::GetFileName($a[3])] = [IO.File]::ReadAllBytes($a[3])
        }
        'download' {
            $name = $a[[Array]::IndexOf($a, '--pattern') + 1]
            $dir = $a[[Array]::IndexOf($a, '--dir') + 1]
            if (!$state.Assets.ContainsKey($name)) { $global:LASTEXITCODE = 1; return }
            $bytes = $state.Assets[$name]
            if ($state.CorruptDownload) { $bytes = [byte[]]@(0) }
            [IO.File]::WriteAllBytes((Join-Path $dir $name), $bytes)
        }
        'edit' {
            if ($a -notcontains '--draft=false' -or $state.Assets.Count -ne 2) { throw 'Premature publication.' }
            $state.Draft = $false
            $state.Published++
        }
        default { throw "Unexpected gh call: $a" }
    }
}

function Reset-Release {
    $script:state = @{
        Exists = $false; Draft = $true; Assets = @{}; Uploads = 0
        FailUpload = 0; CorruptDownload = $false; Published = 0; RemoteCommit = $script:commit
    }
    $global:ttsReleaseTestState = $script:state
}

function Invoke-Publish {
    & "$PSScriptRoot/Publish-Release.ps1" -Repository test/tts-reader -Tag v1.2.3 -Commit $script:commit -AssetDirectory $script:assets
}

function Assert-Failure {
    $failed = $false
    try { Invoke-Publish } catch {
        $failed = $true
        Write-Host "Expected failure: $($_.Exception.Message)"
    }
    if (!$failed) { throw 'Expected publication to fail.' }
}

$script:assets = Join-Path ([IO.Path]::GetTempPath()) ("tts-release-test-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $script:assets | Out-Null
try {
    $exe = Join-Path $script:assets 'tts-reader-windows-amd64.exe'
    [IO.File]::WriteAllBytes($exe, [byte[]]@(77, 90, 1, 2, 3))
    $hash = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$exe.sha256", "$hash  tts-reader-windows-amd64.exe", [Text.Encoding]::ASCII)

    Reset-Release
    $script:state.FailUpload = 2
    Assert-Failure
    if (!$script:state.Draft -or $script:state.Assets.Count -ne 1 -or $script:state.Published -ne 0) {
        throw 'Partial upload left a public release.'
    }
    Invoke-Publish
    if ($script:state.Draft -or $script:state.Published -ne 1) { throw 'Draft recovery failed.' }
    $uploads = $script:state.Uploads
    Invoke-Publish
    if ($script:state.Uploads -ne $uploads -or $script:state.Published -ne 1) { throw 'Rerun modified a published release.' }

    $script:state.Assets['tts-reader-windows-amd64.exe'] = [byte[]]@(0)
    Assert-Failure
    if ($script:state.Uploads -ne $uploads) { throw 'Mismatching public assets were replaced.' }

    Reset-Release
    $script:state.CorruptDownload = $true
    Assert-Failure
    if (!$script:state.Draft -or $script:state.Published -ne 0) { throw 'Corrupt assets were published.' }

    Reset-Release
    $script:state.RemoteCommit = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    Assert-Failure
    if ($script:state.Exists) { throw 'An unverified tag created a release.' }

    Write-Host 'Release publication tests passed: partial failure, recovery, immutable rerun, checksum mismatch, tag mismatch.'
} finally {
    $resolved = (Resolve-Path -LiteralPath $script:assets).Path
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (!$resolved.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unsafe test cleanup path.' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
    Remove-Variable -Name ttsReleaseTestState -Scope Global
}
