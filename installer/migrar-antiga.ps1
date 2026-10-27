# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

<#
.SYNOPSIS
  Ajudante do instalador (livett.iss): prepara a pasta do plugin antes da cópia e limpa a versão anterior depois.

.DESCRIPTION
  -Fase Antes : encerra só o painel do controlador (nunca o vigia de uma LIVE, o MCP de um assistente de IA nem o assistente de
                login), apaga restos .old, renomeia programas em uso para .old (um programa em uso pode ser renomeado, não
                apagado) e, se existe uma pasta de versão anterior (obs-tiktok-live), leva dela as contas e os pareamentos.
  -Fase Depois: retira a pasta da versão anterior sem deixar uma DLL que o OBS ainda carregue.
#>
param(
  [Parameter(Mandatory)][ValidateSet('Antes', 'Depois')][string]$Fase,
  [Parameter(Mandatory)][string]$Novo,
  [Parameter(Mandatory)][string]$Antigo
)
$ErrorActionPreference = 'Continue'

function Remove-PluginFolder([string]$dir) {
  if (-not (Test-Path $dir)) { return }
  Get-ChildItem $dir -Recurse -Filter '*.old' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  Get-ChildItem $dir -Recurse -File -Include '*.exe', '*.dll' -ErrorAction SilentlyContinue | ForEach-Object {
    try { Rename-Item $_.FullName ($_.Name + '.old') -ErrorAction Stop } catch { }
  }
  Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
}

if ($Fase -eq 'Antes') {
  Get-CimInstance Win32_Process -Filter "Name='controller.exe'" -ErrorAction SilentlyContinue |
    Where-Object {
      $_.ExecutablePath -and
      ($_.ExecutablePath.StartsWith($Novo, [StringComparison]::OrdinalIgnoreCase) -or $_.ExecutablePath.StartsWith($Antigo, [StringComparison]::OrdinalIgnoreCase)) -and
      $_.CommandLine -notmatch ' (watchdog|mcp|assist)(\s|$)'
    } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
  Start-Sleep -Milliseconds 800

  # versão anterior: leva os dados (só se a pasta nova ainda não os tem)
  $dadosNovos = Join-Path $Novo 'data\controller\runtime'
  $dadosAntigos = Join-Path $Antigo 'data\controller\runtime'
  if (-not (Test-Path $dadosNovos) -and (Test-Path $dadosAntigos)) {
    New-Item -ItemType Directory -Force (Split-Path $dadosNovos) | Out-Null
    Copy-Item $dadosAntigos $dadosNovos -Recurse -Force
  }

  # prepara a pasta nova para receber os arquivos (programas em uso ficam como .old)
  if (Test-Path $Novo) {
    Get-ChildItem $Novo -Recurse -Filter '*.old' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
    Get-ChildItem $Novo -Recurse -File -Include '*.exe', '*.dll' -ErrorAction SilentlyContinue | ForEach-Object {
      try { Rename-Item $_.FullName ($_.Name + '.old') -ErrorAction Stop } catch { }
    }
  }
} else {
  Remove-PluginFolder $Antigo
}
exit 0
