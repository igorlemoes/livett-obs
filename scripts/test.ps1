# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

<#
.SYNOPSIS
  Roda todos os testes do controlador Go (com o detector de corridas quando há um compilador C).

.NOTES
  O detector de corridas (-race) precisa de gcc no PATH (por exemplo MSYS2 ucrt64: C:\msys64\ucrt64\bin).
  Sem gcc, os testes rodam sem ele.
#>
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$gcc = 'C:\msys64\ucrt64\bin'
if ((Test-Path $gcc) -and -not (Get-Command gcc -ErrorAction SilentlyContinue)) { $env:PATH = "$gcc;$env:PATH" }
Push-Location (Join-Path $root 'controller')
try {
  if (Get-Command gcc -ErrorAction SilentlyContinue) { $env:CGO_ENABLED = '1'; go vet ./... ; go test -race -p 2 ./... -count=1 }
  else { Write-Warning 'gcc não encontrado: rodando sem -race.'; go vet ./... ; go test ./... -count=1 }
  if ($LASTEXITCODE -ne 0) { throw 'Os testes falharam.' }
} finally { Pop-Location }

# Relay da câmera (Worker): só quando as dependências do Node já foram instaladas (cd worker; npm install).
$worker = Join-Path $root 'worker'
if ((Test-Path (Join-Path $worker 'node_modules')) -and (Get-Command npm -ErrorAction SilentlyContinue)) {
  Push-Location $worker
  try { npm test; if ($LASTEXITCODE -ne 0) { throw 'Os testes do relay falharam.' } } finally { Pop-Location }
} else { Write-Warning 'worker\node_modules ausente: testes do relay não rodaram (cd worker; npm install).' }
