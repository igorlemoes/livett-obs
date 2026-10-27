# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

<#
.SYNOPSIS
  Instala (ou remove) o plugin LiveTT no OBS Studio 32.x.

.DESCRIPTION
  Copia a pasta "livett" (ao lado deste script) para C:\ProgramData\obs-studio\plugins\, a pasta de
  plugins que o OBS 32 lê. Preserva os dados da instalação anterior (data\controller\runtime), desbloqueia os
  arquivos baixados da internet e, se faltar permissão, pede elevação (UAC). Também remove uma cópia colocada por
  engano em "C:\Program Files\obs-studio\obs-plugins\livett", onde o OBS NÃO a carrega.

.EXAMPLE
  Instalar.cmd            (duplo clique)
  powershell -File Instalar.ps1 -Remove
#>
param(
  [switch]$Remove,
  [switch]$AceitoOsRiscos,   # já li o aviso de risco (docs\RISCOS.md) e aceito: não pergunta de novo
  [string]$PluginsDir = (Join-Path $env:ProgramData 'obs-studio\plugins'),   # só para testes
  [switch]$NoPause
)
$ErrorActionPreference = 'Stop'
$name = 'livett'
$source = Join-Path $PSScriptRoot $name
$target = Join-Path $PluginsDir $name
$oldName = 'obs-tiktok-live' # nome da pasta nas versões anteriores ao LiveTT: é migrada e removida
$oldTarget = Join-Path $PluginsDir $oldName
$realDir = ($PluginsDir -eq (Join-Path $env:ProgramData 'obs-studio\plugins')) # só a pasta real exige o OBS fechado

function Say([string]$text) { Write-Host $text }
function Finish([int]$code) { if (-not $NoPause) { Read-Host 'Pressione Enter para fechar' | Out-Null }; exit $code }

function Test-Writable([string]$dir) {
  try {
    New-Item -ItemType Directory -Force $dir | Out-Null
    $probe = Join-Path $dir ".write-test-$PID"
    Set-Content $probe 'x'; Remove-Item $probe -Force
    return $true
  } catch { return $false }
}

# 0) aviso de risco: precisa ser aceito de forma explícita (a remoção não precisa)
if (-not $Remove -and -not $AceitoOsRiscos) {
  Say ''
  Say '=============================== AVISO DE RISCO ==============================='
  Say 'Esta ferramenta é INDEPENDENTE e NÃO OFICIAL e PODE VIOLAR os Termos de Serviço do TikTok.'
  Say 'Ela usa serviços internos e não documentados do TikTok, a partir da sua sessão no Chrome.'
  Say 'Isso pode levar à restrição,'
  Say 'suspensão ou banimento da sua conta, e a ferramenta pode parar de funcionar a qualquer momento.'
  Say 'O risco é seu. Teste primeiro com uma conta que você aceite perder. Leia docs\RISCOS.md.'
  Say '================================================================================'
  $answer = Read-Host 'Digite ACEITO para instalar mesmo assim (qualquer outra coisa cancela)'
  if ($answer -ne 'ACEITO') { Say 'Instalação cancelada.'; Finish 1 }
}

# 1) permissão: pede elevação se a pasta de plugins não for gravável
if (-not (Test-Writable $PluginsDir)) {
  $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
  if ($admin) { Say "Sem permissão para gravar em $PluginsDir"; Finish 1 }
  Say 'É preciso permissão de administrador para gravar na pasta de plugins do OBS. Confirme na janela que vai abrir...'
  $args = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$PSCommandPath`"", '-PluginsDir', "`"$PluginsDir`"")
  if ($Remove) { $args += '-Remove' }
  if ($AceitoOsRiscos -or -not $Remove) { $args += '-AceitoOsRiscos' } # quem chegou aqui já aceitou
  Start-Process powershell -Verb RunAs -ArgumentList $args
  exit 0
}

# 2) o OBS precisa estar fechado
if ($realDir -and (Get-Process obs64 -ErrorAction SilentlyContinue)) {
  Say 'O OBS está aberto. Feche o OBS por completo (inclusive o ícone na bandeja) e execute de novo.'
  Finish 1
}

# 2b) um controlador do plugin pode ter ficado vivo depois do OBS fechar (trava os arquivos e segura portas): encerra só
#     o painel ("serve"); nunca o vigia de uma LIVE ("watchdog") nem um assistente de IA em uso ("mcp").
Get-CimInstance Win32_Process -Filter "Name='controller.exe'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -and ($_.ExecutablePath.StartsWith($target, [StringComparison]::OrdinalIgnoreCase) -or $_.ExecutablePath.StartsWith($oldTarget, [StringComparison]::OrdinalIgnoreCase)) -and $_.CommandLine -notmatch ' (watchdog|mcp|assist)(\s|$)' } |
  ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Start-Sleep -Milliseconds 800

# 3) cópia colocada no lugar errado (o OBS não carrega plugins de ...\obs-plugins\<nome>\)
foreach ($n in $name, $oldName) {
  $wrong = Join-Path ${env:ProgramFiles} "obs-studio\obs-plugins\$n"
  if (Test-Path $wrong) {
    try { Remove-Item $wrong -Recurse -Force; Say "Removida a cópia que estava no lugar errado: $wrong" }
    catch { Say "Não consegui remover $wrong. Apague essa pasta manualmente." }
  }
}

# Tira a pasta de uma versão anterior sem deixar uma DLL que o OBS ainda carregue: um programa em uso pode ser renomeado (não apagado).
function Remove-PluginFolder([string]$dir) {
  if (-not (Test-Path $dir)) { return }
  Get-ChildItem $dir -Recurse -Filter '*.old' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  Get-ChildItem $dir -Recurse -File -Include '*.exe', '*.dll' -ErrorAction SilentlyContinue | ForEach-Object {
    try { Rename-Item $_.FullName ($_.Name + '.old') -ErrorAction Stop } catch { }
  }
  Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
}

if ($Remove) {
  $had = (Test-Path $target) -or (Test-Path $oldTarget)
  Remove-PluginFolder $target
  Remove-PluginFolder $oldTarget
  if ($had) { Say "Plugin removido: $target" } else { Say 'O plugin não estava instalado.' }
  Say "Seus dados do navegador ficam em $env:LOCALAPPDATA\TikTokLiveOBS (apague se quiser)."
  Finish 0
}

if (-not (Test-Path (Join-Path $source 'bin\64bit\livett.dll')) -or -not (Test-Path (Join-Path $source 'data\controller\controller.exe'))) {
  Say "Não encontrei a pasta '$name' ao lado deste instalador. Extraia o zip inteiro e execute o instalador de dentro da pasta extraída."
  Finish 1
}

# 4) instala preservando os dados anteriores
$keep = $null
$runtime = Join-Path $target 'data\controller\runtime'
if (-not (Test-Path $runtime)) { $runtime = Join-Path $oldTarget 'data\controller\runtime' } # veio de uma versão anterior: leva as contas e o pareamento
if (Test-Path $runtime) {
  $keep = Join-Path $env:TEMP "tiktoklive-runtime-$PID"
  Copy-Item $runtime $keep -Recurse -Force
}
if (Test-Path $target) {
  # A running program cannot be deleted but can be renamed (an assistant's MCP session may still hold controller.exe).
  Get-ChildItem $target -Recurse -Filter '*.old' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  Get-ChildItem $target -Recurse -File -Include '*.exe', '*.dll' -ErrorAction SilentlyContinue | ForEach-Object {
    try { Rename-Item $_.FullName ($_.Name + '.old') -ErrorAction Stop } catch { }
  }
  Remove-Item $target -Recurse -Force -ErrorAction SilentlyContinue
}
New-Item -ItemType Directory -Force $target | Out-Null # it may still exist (a renamed program in use keeps it)
Copy-Item (Join-Path $source '*') $target -Recurse -Force -ErrorAction Stop
if ($keep) {
  Copy-Item $keep (Join-Path $target 'data\controller\runtime') -Recurse -Force
  Remove-Item $keep -Recurse -Force
}
Get-ChildItem $target -Recurse -File | Unblock-File -ErrorAction SilentlyContinue
if (Test-Path $oldTarget) { Remove-PluginFolder $oldTarget; Say "Versão anterior removida (o LiveTT usa a pasta '$name'): $oldTarget" }

$exe = Join-Path $target 'data\controller\controller.exe'
$version = (& $exe version) 2>$null
Say ''
Say "Instalado: $target"
Say "Controlador: $version"
Say 'Abra o OBS: o painel "LiveTT" aparece à direita (menu Painéis).'
Say 'Se não aparecerem: Ajuda > Arquivos de log > Ver log atual, e procure por "livett".'
Finish 0
