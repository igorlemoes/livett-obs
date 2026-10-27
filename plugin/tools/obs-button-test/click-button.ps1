# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

param([int]$ProcessId, [string]$ButtonName, [int]$TimeoutSec = 20)
Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes
Add-Type @"
using System; using System.Runtime.InteropServices;
public class M {
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, uint d, UIntPtr e);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
}
"@
$root = [Windows.Automation.AutomationElement]::RootElement
$pidCond = New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::ProcessIdProperty, $ProcessId)
$nameCond = New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::NameProperty, $ButtonName)
$deadline = (Get-Date).AddSeconds($TimeoutSec)
while ((Get-Date) -lt $deadline) {
  $b = $root.FindFirst([Windows.Automation.TreeScope]::Descendants, (New-Object Windows.Automation.AndCondition($pidCond, $nameCond)))
  if ($b) {
    $win = $b; while ($win -and $win.Current.ControlType.ProgrammaticName -ne 'ControlType.Window') { $win = [Windows.Automation.TreeWalker]::ControlViewWalker.GetParent($win) }
    if ($win) { [M]::SetForegroundWindow([IntPtr]$win.Current.NativeWindowHandle) | Out-Null }
    Start-Sleep -Milliseconds 400
    $r = $b.Current.BoundingRectangle
    [M]::SetCursorPos([int]($r.X + $r.Width / 2), [int]($r.Y + $r.Height / 2)) | Out-Null
    Start-Sleep -Milliseconds 200
    [M]::mouse_event(0x0002, 0, 0, 0, [UIntPtr]::Zero); Start-Sleep -Milliseconds 80; [M]::mouse_event(0x0004, 0, 0, 0, [UIntPtr]::Zero)
    "clique de mouse em '$ButtonName' ($([int]($r.X + $r.Width / 2)),$([int]($r.Y + $r.Height / 2)))"
    return
  }
  Start-Sleep -Milliseconds 300
}
"botão '$ButtonName' não encontrado em ${TimeoutSec}s"; exit 1
