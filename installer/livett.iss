; SPDX-License-Identifier: GPL-2.0-or-later
; Copyright (C) 2026 Igor Lemões
;
; Instalador do LiveTT (Windows x64), feito com o Inno Setup 6.
; Compilar: powershell -File scripts\build.ps1 -Instalador   (gera dist\LiveTT-Setup-<versão>.exe)
; Instalação silenciosa: LiveTT-Setup-<versão>.exe /VERYSILENT /ACEITO=1   (/ACEITO=1 confirma que você leu docs\RISCOS.md)

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "..\dist\livett"
#endif

[Setup]
; Este GUID identifica o programa no Windows (atualizações e desinstalação): não mude.
AppId={{B7C3F2D0-6E1A-4C4B-9A52-5E8D1F0A7C31}
AppName=LiveTT
AppVersion={#AppVersion}
AppPublisher=Igor Lemões
AppPublisherURL=https://github.com/igorlemoes/livett-obs
AppSupportURL=https://github.com/igorlemoes/livett-obs/issues
AppUpdatesURL=https://github.com/igorlemoes/livett-obs/releases
VersionInfoVersion={#AppVersion}
VersionInfoDescription=Instalador do LiveTT
; O OBS 32 só carrega plugins desta pasta.
DefaultDirName={commonappdata}\obs-studio\plugins\livett
DisableDirPage=yes
DisableProgramGroupPage=yes
UsePreviousAppDir=no
DirExistsWarning=no
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir=..\dist
OutputBaseFilename=LiveTT-Setup-{#AppVersion}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
; Primeiro o aviso de risco (com aceite explícito), depois o texto da licença.
LicenseFile=aviso-de-risco.txt
InfoBeforeFile=..\LICENSE
UninstallDisplayName=LiveTT
CloseApplications=no
RestartApplications=no
ShowLanguageDialog=no

[Languages]
Name: "ptbr"; MessagesFile: "compiler:Languages\BrazilianPortuguese.isl"

[Files]
Source: "{#SourceDir}\*"; DestDir: "{app}"; Flags: recursesubdirs createallsubdirs ignoreversion
; Versão anterior (pasta obs-tiktok-live): leva as contas e os pareamentos, só se a pasta nova ainda não os tem.
Source: "{code:OldRuntimeDir}\*"; DestDir: "{app}\data\controller\runtime"; Flags: external recursesubdirs createallsubdirs onlyifdoesntexist skipifsourcedoesntexist uninsneveruninstall; Check: NeedMigrate

; O OBS e o controlador rodam como o usuário comum, que em ProgramData só cria arquivos novos: sem isto, quem instala como administrador deixa pastas onde o programa não consegue gravar ("Acesso negado").
[Dirs]
Name: "{app}\data\controller\runtime"; Permissions: users-modify
Name: "{app}\data\controller\media"; Permissions: users-modify

[Run]
Filename: "{app}\LEIA-ME.txt"; Description: "Abrir o LEIA-ME"; Flags: postinstall shellexec skipifsilent unchecked

[Code]
function CustomDir: Boolean;
begin
  // /DIR=... só é usado em testes: aí o instalador não mexe no OBS de verdade.
  Result := ExpandConstant('{param:DIR|}') <> '';
end;

function IsRunning(const ExeName: String): Boolean;
var
  Locator, Service, Found: Variant;
begin
  Result := False;
  try
    Locator := CreateOleObject('WbemScripting.SWbemLocator');
    Service := Locator.ConnectServer('.', 'root\cimv2');
    Found := Service.ExecQuery('SELECT ProcessId FROM Win32_Process WHERE Name = ''' + ExeName + '''');
    Result := Found.Count > 0;
  except
    Result := False;
  end;
end;

function OldDir: String;
begin
  Result := ExtractFilePath(RemoveBackslash(ExpandConstant('{app}'))) + 'obs-tiktok-live';
end;

function OldRuntimeDir(Param: String): String;
begin
  Result := OldDir + '\data\controller\runtime';
end;

function NeedMigrate: Boolean;
begin
  Result := DirExists(OldRuntimeDir('')) and not DirExists(ExpandConstant('{app}\data\controller\runtime'));
end;

// Mode 0: apaga os restos .old. Mode 1: renomeia os programas (exe/dll) para .old, porque um programa em uso pode ser renomeado, mas não apagado.
procedure Sweep(const Dir: String; Mode: Integer);
var
  R: TFindRec;
  Path, Ext: String;
begin
  if not DirExists(Dir) then Exit;
  if FindFirst(Dir + '\*', R) then begin
    try
      repeat
        if (R.Name <> '.') and (R.Name <> '..') then begin
          Path := Dir + '\' + R.Name;
          Ext := Lowercase(ExtractFileExt(R.Name));
          if R.Attributes and FILE_ATTRIBUTE_DIRECTORY <> 0 then
            Sweep(Path, Mode)
          else if Mode = 0 then begin
            if Ext = '.old' then DeleteFile(Path);
          end else if ((Ext = '.exe') or (Ext = '.dll')) and (Pos('unins', Lowercase(R.Name)) <> 1) then
            RenameFile(Path, Path + '.old');
        end;
      until not FindNext(R);
    finally
      FindClose(R);
    end;
  end;
end;

// Encerra só o painel do controlador (nunca o vigia de uma LIVE, o MCP de um assistente de IA nem o assistente de login).
procedure StopControllers;
var
  Locator, Service, List, P: Variant;
  I: Integer;
  Exe, Cmd, Mine, Old: String;
begin
  Mine := Lowercase(RemoveBackslash(ExpandConstant('{app}')));
  Old := Lowercase(OldDir);
  try
    Locator := CreateOleObject('WbemScripting.SWbemLocator');
    Service := Locator.ConnectServer('.', 'root\cimv2');
    List := Service.ExecQuery('SELECT * FROM Win32_Process WHERE Name = ''controller.exe''');
    for I := 0 to List.Count - 1 do begin
      P := List.ItemIndex(I);
      if VarIsNull(P.ExecutablePath) then Continue;
      Exe := P.ExecutablePath;
      Exe := Lowercase(Exe);
      if (Pos(Mine, Exe) <> 1) and (Pos(Old, Exe) <> 1) then Continue;
      Cmd := '';
      if not VarIsNull(P.CommandLine) then Cmd := P.CommandLine;
      Cmd := Lowercase(Cmd);
      if (Pos(' watchdog', Cmd) > 0) or (Pos(' mcp', Cmd) > 0) or (Pos(' assist', Cmd) > 0) then Continue;
      P.Terminate;
    end;
  except
    Log('Não consegui listar os processos do controlador.');
  end;
  Sleep(800);
end;

function InitializeSetup: Boolean;
begin
  Result := True;
  if WizardSilent and (ExpandConstant('{param:ACEITO|0}') <> '1') then begin
    Log('Instalação silenciosa sem /ACEITO=1: o aviso de risco precisa ser aceito.');
    SuppressibleMsgBox('Na instalação silenciosa passe /ACEITO=1 para confirmar que você leu o aviso de risco (docs\RISCOS.md).', mbError, MB_OK, IDOK);
    Result := False;
    Exit;
  end;
  if (not CustomDir) and IsRunning('obs64.exe') then begin
    SuppressibleMsgBox('O OBS está aberto. Feche o OBS por completo (inclusive o ícone na bandeja) e execute o instalador de novo.', mbError, MB_OK, IDOK);
    Result := False;
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssInstall then begin
    StopControllers;
    Sweep(ExpandConstant('{app}'), 0);
    Sweep(ExpandConstant('{app}'), 1);
  end else if CurStep = ssPostInstall then begin
    Sweep(ExpandConstant('{app}'), 0);
    // Tira a pasta da versão anterior sem deixar uma DLL que o OBS ainda carregue.
    Sweep(OldDir, 0);
    Sweep(OldDir, 1);
    DelTree(OldDir, True, True, True);
  end;
end;

function InitializeUninstall: Boolean;
begin
  Result := True;
  if IsRunning('obs64.exe') then begin
    SuppressibleMsgBox('O OBS está aberto. Feche o OBS por completo e execute a desinstalação de novo.', mbError, MB_OK, IDOK);
    Result := False;
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then begin
    // Os dados do usuário (contas salvas, pareamentos das câmeras e o login do Chrome) só saem se ele quiser.
    if SuppressibleMsgBox('Apagar também seus dados do LiveTT (contas salvas, pareamentos das câmeras e o login do navegador)?'#13#10#13#10 +
        'Se você pretende instalar de novo, responda Não.', mbConfirmation, MB_YESNO or MB_DEFBUTTON2, IDNO) = IDYES then begin
      DelTree(ExpandConstant('{app}'), True, True, True);
      DelTree(ExpandConstant('{localappdata}\TikTokLiveOBS'), True, True, True);
    end;
  end;
end;
