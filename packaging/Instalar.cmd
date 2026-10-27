@echo off
rem Instala o plugin LiveTT no OBS (duplo clique). Use "Instalar.cmd /remover" para remover.
setlocal
if /i "%~1"=="/remover" (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0Instalar.ps1" -Remove
) else (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0Instalar.ps1"
)
