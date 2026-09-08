param(
    [Parameter(Mandatory)][ValidatePattern('^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')][string]$Repository,
    [Parameter(Mandatory)][ValidatePattern('^v[0-9]+\.[0-9]+\.[0-9]+$')][string]$Tag,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{40}$')][string]$Commit,
    [string]$AssetDirectory = 'dist'
)

$ErrorActionPreference = 'Stop'

function Invoke-Gh {
    param([string[]]$Arguments)
    $output = & gh @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "GitHub CLI failed: gh $($Arguments -join ' ')"
    }
    return $output
}

function Assert-ReleaseCommit {
    $remoteCommit = Invoke-Gh -Arguments @('api', "repos/$Repository/commits/$Tag", '--jq', '.sha')
    if ($remoteCommit -cne $Commit) {
        throw "Remote tag $Tag does not point to verified commit $Commit."
    }
}

$exeName = 'tts-reader-windows-amd64.exe'
$checksumName = "$exeName.sha256"
$assetNames = @($exeName, $checksumName)
$assetDirectoryPath = (Resolve-Path -LiteralPath $AssetDirectory).Path
$localHash = (Get-FileHash -LiteralPath (Join-Path $assetDirectoryPath $exeName) -Algorithm SHA256).Hash.ToLowerInvariant()
$checksumPath = Join-Path $assetDirectoryPath $checksumName
$checksumText = [IO.File]::ReadAllText($checksumPath).Trim()
if ($checksumText -cne "$localHash  $exeName") {
    throw 'Local checksum does not match the executable.'
}

Assert-ReleaseCommit
$releaseJSON = & gh release view $Tag --repo $Repository --json isDraft
if ($LASTEXITCODE -ne 0) {
    $notes = "Windows amd64 build for Audiobook TTS Reader.`n`nVerified commit: $Commit"
    Invoke-Gh -Arguments @('release', 'create', $Tag, '--repo', $Repository, '--verify-tag', '--draft', '--title', $Tag, '--notes', $notes) | Out-Null
    $releaseJSON = Invoke-Gh -Arguments @('release', 'view', $Tag, '--repo', $Repository, '--json', 'isDraft')
}
$release = $releaseJSON | ConvertFrom-Json
if ($release.isDraft -isnot [bool]) {
    throw 'GitHub returned an invalid release state.'
}

# Замінювати артефакти можна лише в чернетці; опублікований реліз незмінний.
if ($release.isDraft) {
    foreach ($name in $assetNames) {
        Invoke-Gh -Arguments @('release', 'upload', $Tag, (Join-Path $assetDirectoryPath $name), '--repo', $Repository, '--clobber') | Out-Null
    }
}

$downloadDirectory = Join-Path ([IO.Path]::GetTempPath()) ("tts-release-verify-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $downloadDirectory | Out-Null
try {
    foreach ($name in $assetNames) {
        Invoke-Gh -Arguments @('release', 'download', $Tag, '--repo', $Repository, '--pattern', $name, '--dir', $downloadDirectory) | Out-Null
        $downloadedHash = (Get-FileHash -LiteralPath (Join-Path $downloadDirectory $name) -Algorithm SHA256).Hash
        $expectedHash = (Get-FileHash -LiteralPath (Join-Path $assetDirectoryPath $name) -Algorithm SHA256).Hash
        if ($downloadedHash -cne $expectedHash) {
            throw "Downloaded asset differs from the verified build: $name"
        }
    }
    Assert-ReleaseCommit
    if ($release.isDraft) {
        Invoke-Gh -Arguments @('release', 'edit', $Tag, '--repo', $Repository, '--draft=false') | Out-Null
    }
    Write-Host "Release $Tag verified at $Commit (SHA256 $localHash)."
} finally {
    # Видаляємо лише унікальний тимчасовий каталог, створений цим запуском.
    $resolved = (Resolve-Path -LiteralPath $downloadDirectory).Path
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (!$resolved.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Refusing to remove a directory outside the temporary root.'
    }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
