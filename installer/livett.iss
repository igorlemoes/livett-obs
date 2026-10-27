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
Source: "migrar-antiga.ps1"; Flags: dontcopy

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

// O ajudante (migrar-antiga.ps1) para o controlador do plugin, tira o que atrapalha a cópia e leva os dados de uma versão anterior.
procedure RunHelper(const Phase: String);
var
  Script, NewDir, OldDir, Params: String;
  Code: Integer;
begin
  ExtractTemporaryFile('migrar-antiga.ps1');
  Script := ExpandConstant('{tmp}\migrar-antiga.ps1');
  NewDir := RemoveBackslash(WizardDirValue);
  OldDir := ExtractFilePath(NewDir) + 'obs-tiktok-live';
  Params := '-NoProfile -ExecutionPolicy Bypass -File "' + Script + '" -Fase ' + Phase + ' -Novo "' + NewDir + '" -Antigo "' + OldDir + '"';
  Exec(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'), Params, '', SW_HIDE, ewWaitUntilTerminated, Code);
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
  if CurStep = ssInstall then
    RunHelper('Antes')
  else if CurStep = ssPostInstall then
    RunHelper('Depois');
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
