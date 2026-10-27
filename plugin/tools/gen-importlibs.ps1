# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

<#
.SYNOPSIS
  Cria as bibliotecas de importação MSVC (.lib) das DLLs do OBS contra as quais o plugin é ligado.

.DESCRIPTION
  O OBS não distribui arquivos .lib. Eles são derivados das exportações da DLL instalada (a mesma versão em que o
  plugin vai rodar), de modo que o plugin se liga exatamente à ABI do OBS de destino.

.EXAMPLE
  powershell -File plugin\tools\gen-importlibs.ps1
  powershell -File plugin\tools\gen-importlibs.ps1 -ObsBin 'D:\obs\bin\64bit'
#>
param(
  [string]$ObsBin = 'C:\Program Files\obs-studio\bin\64bit',
  [string]$Out = (Join-Path $PSScriptRoot '..\build\importlibs')
)
$ErrorActionPreference = 'Stop'
$vcvars = 'C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvars64.bat'
if (-not (Test-Path $vcvars)) { throw "Não encontrei o Visual Studio Build Tools 2022 em: $vcvars" }
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path

function Invoke-Vc([string]$Command) {
  # vcvars64.bat prints a harmless "vswhere.exe" warning on stderr; only the exit code decides.
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try { $text = cmd /s /c "`"$vcvars`" >nul && $Command" 2>&1 | Out-String } finally { $ErrorActionPreference = $previous }
  if ($LASTEXITCODE -ne 0) { throw "Falhou: $Command`n$text" }
  return $text
}

foreach ($name in 'obs', 'obs-frontend-api') {
  $dll = Join-Path $ObsBin "$name.dll"
  if (-not (Test-Path $dll)) { throw "DLL não encontrada: $dll" }
  $dump = Invoke-Vc "dumpbin /exports `"$dll`""
  $symbols = foreach ($line in $dump -split "`r?`n") {
    if ($line -match '^\s+\d+\s+[0-9A-F]+\s+[0-9A-F]{8}\s+(\S+)') { $Matches[1] }
  }
  if (-not $symbols) { throw "Nenhuma exportação lida de $name.dll" }
  $def = Join-Path $Out "$name.def"
  Set-Content -Path $def -Encoding ascii -Value (@("LIBRARY $name", 'EXPORTS') + $symbols)
  Invoke-Vc "lib /nologo /def:`"$def`" /machine:x64 /out:`"$(Join-Path $Out "$name.lib")`"" | Out-Null
  "{0}.lib: {1} símbolos" -f $name, @($symbols).Count
}
