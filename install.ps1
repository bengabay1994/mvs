#Requires -Version 5.1

[CmdletBinding()]
param(
    [string]$Prefix = (Join-Path $HOME ".local"),
    [switch]$AddToPath
)

$ErrorActionPreference = "Stop"
$Prefix = [IO.Path]::GetFullPath($Prefix)
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

function Add-UserPathEntry {
    param([string]$Entry)

    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment", $true)
    if (-not $key) {
        throw "Unable to open HKCU\Environment for writing."
    }

    try {
        $hasPath = $key.GetValueNames() -contains "Path"
        $rawPath = [string]$key.GetValue(
            "Path",
            "",
            [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
        )
        if (Test-PathEntry -PathValue $rawPath -Entry $Entry) {
            return
        }

        $kind = if ($hasPath) {
            $key.GetValueKind("Path")
        }
        else {
            [Microsoft.Win32.RegistryValueKind]::ExpandString
        }
        if ($kind -notin @(
                [Microsoft.Win32.RegistryValueKind]::String,
                [Microsoft.Win32.RegistryValueKind]::ExpandString
            )) {
            $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        }

        $entries = @($rawPath.TrimEnd(';'), $Entry) |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
        $key.SetValue("Path", ($entries -join ';'), $kind)
    }
    finally {
        $key.Dispose()
    }

    if (-not ("Mvs.NativeMethods" -as [type])) {
        Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

namespace Mvs {
    public static class NativeMethods {
        [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr SendMessageTimeout(
            IntPtr hWnd,
            uint message,
            UIntPtr wParam,
            string lParam,
            uint flags,
            uint timeout,
            out UIntPtr result);
    }
}
"@
    }

    $result = [UIntPtr]::Zero
    [void][Mvs.NativeMethods]::SendMessageTimeout(
        [IntPtr]0xffff,
        0x001a,
        [UIntPtr]::Zero,
        "Environment",
        0x0002,
        5000,
        [ref]$result
    )
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

$processHasBinDir = Test-PathEntry -PathValue $env:Path -Entry $binDir
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
$persistentHasBinDir = (
        (Test-PathEntry -PathValue $userPath -Entry $binDir) -or
        (Test-PathEntry -PathValue $machinePath -Entry $binDir)
    )

if (-not $persistentHasBinDir) {
    if ($AddToPath) {
        Add-UserPathEntry -Entry $binDir
        if (-not $processHasBinDir) {
            $env:Path += ";$binDir"
        }
        Write-Host "added to user PATH: $binDir"
    }
    else {
        $escapedScriptPath = $PSCommandPath.Replace("'", "''")
        $escapedPrefix = $Prefix.Replace("'", "''")
        Write-Warning "$binDir is not on your persistent user or machine PATH."
        Write-Host @"
Run this command to add it permanently to your user PATH:

    & '$escapedScriptPath' -Prefix '$escapedPrefix' -AddToPath

"@
    }
}
elseif (-not $processHasBinDir) {
    Write-Warning "$binDir is already on your persistent PATH. Open a new terminal to use mvs."
}

Write-Host "run ``mvs doctor`` to verify which agents were detected."
