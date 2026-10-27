# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

param([int]$ProcessId, [string]$WindowTitle, [string]$Button, [int]$TimeoutSec = 25)
Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes
$deadline = (Get-Date).AddSeconds($TimeoutSec)
$root = [Windows.Automation.AutomationElement]::RootElement
$pidCond = New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::ProcessIdProperty, $ProcessId)
while ((Get-Date) -lt $deadline) {
  $nameCond = New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::NameProperty, $WindowTitle)
  $both = New-Object Windows.Automation.AndCondition($pidCond, $nameCond)
  $windows = $root.FindAll([Windows.Automation.TreeScope]::Descendants, $both)
  foreach ($w in $windows) {
    if ($w.Current.Name -like $WindowTitle) {
      $btn = $w.FindFirst([Windows.Automation.TreeScope]::Descendants, (New-Object Windows.Automation.AndCondition(
        (New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::NameProperty, $Button)),
        (New-Object Windows.Automation.PropertyCondition([Windows.Automation.AutomationElement]::ControlTypeProperty, [Windows.Automation.ControlType]::Button))))
      )
      if ($btn) {
        $names = ($w.FindAll([Windows.Automation.TreeScope]::Descendants, [Windows.Automation.Condition]::TrueCondition) | % { $_.Current.Name } | ? { $_ } | select -first 12) -join ' | '
        "janela '$($w.Current.Name)': $names"
        if (-not $btn.Current.IsEnabled) { Start-Sleep -Milliseconds 400; continue }   # waits until the button is enabled
        $btn.GetCurrentPattern([Windows.Automation.InvokePattern]::Pattern).Invoke()
        "clicou em '$Button'"
        return
      }
    }
  }
  Start-Sleep -Milliseconds 300
}
"janela '$WindowTitle' com botão '$Button' não apareceu em ${TimeoutSec}s"
$windows = $root.FindAll([Windows.Automation.TreeScope]::Children, $pidCond); "janelas do processo: " + (($windows | % { $_.Current.Name }) -join ' | ')
exit 1
