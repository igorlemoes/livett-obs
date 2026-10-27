# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

param([int]$ProcessId)
Add-Type @"
using System; using System.Text; using System.Collections.Generic; using System.Runtime.InteropServices;
public class W {
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc p, IntPtr l);
  [DllImport("user32.dll")] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern bool IsIconic(IntPtr h);
  [DllImport("user32.dll")] static extern IntPtr GetWindow(IntPtr h, uint cmd);
  public static List<string> ForPid(uint pid) {
    var r = new List<string>();
    EnumWindows((h, l) => { uint p; GetWindowThreadProcessId(h, out p); if (p == pid) {
      var t = new StringBuilder(256); GetWindowText(h, t, 256); var c = new StringBuilder(128); GetClassName(h, c, 128);
      r.Add(string.Format("{0} | '{1}' | visivel={2} | minimizada={3} | dona={4}", c, t, IsWindowVisible(h), IsIconic(h), GetWindow(h, 4) != IntPtr.Zero)); }
      return true; }, IntPtr.Zero);
    return r;
  }
}
"@
[W]::ForPid($ProcessId) | ? { $_ -notmatch "^(MSCTFIME|IME|QEventDispatcher|Qt\d*ClipboardView|CiceroUIWndFrame|GDI\+|Default IME|QTrayIconMessageWindow|DDE)" } | select -first 30
