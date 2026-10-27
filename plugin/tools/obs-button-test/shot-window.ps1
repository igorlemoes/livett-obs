# SPDX-License-Identifier: GPL-2.0-or-later
# Copyright (C) 2026 Igor Lemões

param([int]$ProcessId, [string]$Title, [string]$Out)
Add-Type -AssemblyName System.Drawing
Add-Type -ReferencedAssemblies System.Drawing @"
using System; using System.Text; using System.Drawing; using System.Runtime.InteropServices;
public class SW {
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc p, IntPtr l);
  [DllImport("user32.dll")] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] static extern bool PrintWindow(IntPtr h, IntPtr dc, uint f);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
  public static string Shot(uint pid, string title, string path) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((h, l) => { uint p; GetWindowThreadProcessId(h, out p); if (p == pid) { var t = new StringBuilder(256); GetWindowText(h, t, 256); if (t.ToString() == title) { found = h; return false; } } return true; }, IntPtr.Zero);
    if (found == IntPtr.Zero) return "janela não encontrada";
    RECT r; GetWindowRect(found, out r); int w = r.R - r.L, hh = r.B - r.T;
    using (var b = new Bitmap(w, hh)) { using (var g = Graphics.FromImage(b)) { IntPtr dc = g.GetHdc(); PrintWindow(found, dc, 2); g.ReleaseHdc(dc); } b.Save(path, System.Drawing.Imaging.ImageFormat.Png); }
    return w + "x" + hh;
  }
}
"@
[SW]::Shot([uint32]$ProcessId, $Title, $Out)
