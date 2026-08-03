#Requires -Version 5.1

[CmdletBinding()]
param(
    [string]$Prefix = (Join-Path $HOME ".local")
)

$ErrorActionPreference = "Stop"
$binDir = Join-Path $Prefix "bin"
$buildOutput = Join-Path $PSScriptRoot "mvs.exe"
$destination = Join-Path $binDir "mvs.exe"

function Test-PathEntry {
    param(
        [AllowNull()]
        [string]$PathValue,
        [string]$Entry
    )

    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return $false
    }

    $target = [IO.Path]::GetFullPath($Entry).TrimEnd([char[]]"\/")
    foreach ($item in $PathValue -split [IO.Path]::PathSeparator) {
        if ([string]::IsNullOrWhiteSpace($item)) {
            continue
        }

        try {
            $expanded = [Environment]::ExpandEnvironmentVariables($item.Trim().Trim('"'))
            $candidate = [IO.Path]::GetFullPath($expanded).TrimEnd([char[]]"\/")
            if ($candidate.Equals($target, [StringComparison]::OrdinalIgnoreCase)) {
                return $true
            }
        }
        catch {
            # Ignore malformed PATH entries and continue checking the rest.
        }
    }

    return $false
}

$mise = Get-Command mise -ErrorAction SilentlyContinue
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $mise -and -not $go) {
    Write-Error @"
Go is required to build mvs from source.
Install Go with mise (mise use go@latest), or download a prebuilt binary.
"@
}

Write-Host "building mvs..."
Push-Location $PSScriptRoot
try {
    if ($mise) {
        & mise exec -- go build -trimpath -o $buildOutput .
    }
    else {
        & go build -trimpath -o $buildOutput .
    }

    if ($LASTEXITCODE -ne 0) {
        throw "Go build failed with exit code $LASTEXITCODE."
    }
}
finally {
    Pop-Location
}

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
Copy-Item -Force $buildOutput $destination
Write-Host "installed: $destination"

if (-not (Test-PathEntry -PathValue $env:Path -Entry $binDir)) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (Test-PathEntry -PathValue $userPath -Entry $binDir) {
        Write-Warning "$binDir is already in your user PATH. Open a new terminal to use mvs."
    }
    else {
        $escapedBinDir = $binDir.Replace("'", "''")
        Write-Warning "$binDir is not on your PATH."
        Write-Host @"
Run these commands to add it permanently to your user PATH and use it now:

    `$binDir = '$escapedBinDir'
    `$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    `$newPath = @(`$userPath, `$binDir) | Where-Object { -not [string]::IsNullOrWhiteSpace(`$_) }
    [Environment]::SetEnvironmentVariable('Path', (`$newPath -join ';'), 'User')
    `$env:Path += ";`$binDir"

"@
    }
}

Write-Host "run ``mvs doctor`` to verify which agents were detected."
