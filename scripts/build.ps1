# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

<#
.SYNOPSIS
  Gera o pacote de instalação por extração: dist\LiveTT-OBS-plugin-<versão>.zip (+ SHA256SUMS.txt).

.DESCRIPTION
  1. (opcional) roda os testes do controlador Go;
  2. compila o plugin nativo do OBS (plugin\tools\build.cmd; precisa das dependências de plugin\README.md);
  3. compila o controlador Go (-trimpath: sem caminhos da máquina de build no executável);
  4. monta livett\{bin\64bit\*.dll, data\controller\controller.exe, LEIA-ME.txt} e zipa.

  Requisitos: Go 1.24+, Visual Studio 2022 Build Tools + CMake + Ninja (só para o plugin).

.EXAMPLE
  powershell -File scripts\build.ps1            # compila tudo e empacota
  powershell -File scripts\build.ps1 -Test      # roda os testes antes
  powershell -File scripts\build.ps1 -SkipPlugin # reaproveita a DLL já compilada
  powershell -File scripts\build.ps1 -Instalador # também gera dist\LiveTT-Setup-<versão>.exe (precisa do Inno Setup 6)
#>
param(
  [switch]$Test,
  [switch]$SkipPlugin,
  [switch]$Instalador
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$version = (Get-Content (Join-Path $root 'VERSION') -Raw).Trim()
$dist = Join-Path $root 'dist'
$package = Join-Path $dist 'livett'

if ($Test) { & (Join-Path $PSScriptRoot 'test.ps1') }

if (-not $SkipPlugin) {
  Push-Location $root
  try {
    cmd /c "plugin\tools\build.cmd"
    if ($LASTEXITCODE -ne 0) { throw 'A compilação do plugin falhou.' }
  } finally { Pop-Location }
}
$dll = Join-Path $root 'plugin\build\livett\livett.dll'
if (-not (Test-Path $dll)) { throw "DLL não encontrada: $dll (compile o plugin; veja plugin\README.md)" }

Remove-Item $package -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force (Join-Path $package 'bin\64bit'), (Join-Path $package 'data\controller') | Out-Null
Copy-Item $dll (Join-Path $package 'bin\64bit\')

Push-Location (Join-Path $root 'controller')
try {
  go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$version" -o (Join-Path $package 'data\controller\controller.exe') .\cmd\controller
  if ($LASTEXITCODE -ne 0) { throw 'A compilação do controlador falhou.' }
} finally { Pop-Location }

(Get-Content (Join-Path $root 'packaging\LEIA-ME.txt') -Raw).Replace('{{VERSION}}', $version) |
  Set-Content (Join-Path $package 'LEIA-ME.txt') -Encoding utf8

# Docs for people and assistants (MCP guide, safety rules, operation) travel with the plugin.
New-Item -ItemType Directory -Force (Join-Path $package 'docs') | Out-Null
foreach ($doc in 'MCP.md', 'SEGURANCA.md', 'OPERACAO.md', 'RISCOS.md') { Copy-Item (Join-Path $root "docs\$doc") (Join-Path $package 'docs\') }
Copy-Item (Join-Path $root 'AGENTS.md') (Join-Path $package 'docs\AGENTS.md')
Copy-Item (Join-Path $root 'LICENSE') (Join-Path $package 'LICENSE')
Copy-Item (Join-Path $root 'THIRD-PARTY-NOTICES.txt') (Join-Path $package 'THIRD-PARTY-NOTICES.txt')

$zip = Join-Path $dist "LiveTT-OBS-plugin-$version.zip"
Remove-Item $zip -Force -ErrorAction SilentlyContinue
# Not Compress-Archive: Windows PowerShell 5.1 writes backslashes in entry names, which breaks other extractors.
Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
$archive = [System.IO.Compression.ZipFile]::Open($zip, 'Create')
try {
  foreach ($file in Get-ChildItem $package -Recurse -File) {
    $entry = 'livett/' + $file.FullName.Substring($package.Length + 1).Replace('\', '/')
    [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, $file.FullName, $entry, 'Optimal') | Out-Null
  }
  [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, (Join-Path $package 'LEIA-ME.txt'), 'LEIA-ME.txt', 'Optimal') | Out-Null
  foreach ($name in 'Instalar.cmd', 'Instalar.ps1', 'Testar-MCP.ps1') {
    [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, (Join-Path $root "packaging\$name"), $name, 'Optimal') | Out-Null
  }
} finally { $archive.Dispose() }
$hash = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
$sums = @("$hash  $(Split-Path $zip -Leaf)")

if ($Instalador) {
  $iscc = @("$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe", "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe") | Where-Object { Test-Path $_ } | Select-Object -First 1
  if (-not $iscc) { throw 'Inno Setup 6 não encontrado (winget install JRSoftware.InnoSetup).' }
  & $iscc "/DAppVersion=$version" "/DSourceDir=$package" (Join-Path $root 'installer\livett.iss')
  if ($LASTEXITCODE -ne 0) { throw 'A compilação do instalador falhou.' }
  $setup = Join-Path $dist "LiveTT-Setup-$version.exe"
  $sums += "$((Get-FileHash $setup -Algorithm SHA256).Hash.ToLower())  $(Split-Path $setup -Leaf)"
  "{0}  ({1:N1} MB)" -f $setup, ((Get-Item $setup).Length / 1MB)
}
$sums | Set-Content (Join-Path $dist 'SHA256SUMS.txt') -Encoding ascii
"{0}  ({1:N1} MB)`nSHA-256: {2}" -f $zip, ((Get-Item $zip).Length / 1MB), $hash
