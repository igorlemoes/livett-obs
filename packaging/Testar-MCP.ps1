# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

# Testa o servidor MCP do LiveTT (SÓ LEITURA: não publica nada, não envia mensagens).
# Uso: powershell -ExecutionPolicy Bypass -File Testar-MCP.ps1 [-Root <pasta do controlador>]
param([string]$Root = 'C:\ProgramData\obs-studio\plugins\livett\data\controller')
$exe = Join-Path $Root 'controller.exe'
if (-not (Test-Path $exe)) { Write-Host "controller.exe não encontrado em $Root. Instale o plugin primeiro (Instalar.cmd)."; exit 1 }

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $exe
$psi.Arguments = "mcp --root `"$Root`""
$psi.RedirectStandardInput = $true
$psi.RedirectStandardOutput = $true
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$p = [System.Diagnostics.Process]::Start($psi)
function Send($o) { $p.StandardInput.WriteLine(($o | ConvertTo-Json -Depth 20 -Compress)); $p.StandardInput.Flush() }
function Recv { $p.StandardOutput.ReadLine() }

Send @{ jsonrpc = '2.0'; id = 1; method = 'initialize'; params = @{ protocolVersion = '2024-11-05'; capabilities = @{}; clientInfo = @{ name = 'testar-mcp'; version = '1' } } }
$init = (Recv | ConvertFrom-Json).result
if ($init.serverInfo.name) { Write-Host "Servidor: $($init.serverInfo.name) $($init.serverInfo.version)" }
Send @{ jsonrpc = '2.0'; method = 'notifications/initialized' }

Send @{ jsonrpc = '2.0'; id = 2; method = 'tools/list' }
$tools = (Recv | ConvertFrom-Json).result.tools
Write-Host "Ferramentas ($($tools.Count)): $(($tools | ForEach-Object name) -join ', ')"

Send @{ jsonrpc = '2.0'; id = 3; method = 'tools/call'; params = @{ name = 'live_status'; arguments = @{} } }
$status = (Recv | ConvertFrom-Json).result.content[0].text | ConvertFrom-Json
Write-Host "Fase: $($status.fase) — $($status.detalhe)"
if ($status.fase -eq 'disconnected') { Write-Host 'Abra o OBS com o plugin (o painel conecta sozinho) e rode de novo.' }

$p.StandardInput.Close()
if (-not $p.WaitForExit(5000)) { $p.Kill() }
