; ButterCams installer (Inno Setup 6).
;
; Build:   iscc /DAppVersion=1.0.0 installer\buttercams.iss
; Needs:   dist\buttercams.exe built first (see installer\build.ps1).
;
; The wizard only collects answers. Everything that touches the system is
; done by "buttercams.exe setup ...", the same code the tests cover:
;   setup init               data root, config.json, first administrator, certificate
;   setup install-service    service (delayed auto start, restart on failure), firewall rule, start
;   setup uninstall-service  stop and remove the service; optionally the data root
;   setup test-destination   the Test button on the destination page

#ifndef AppVersion
  #define AppVersion "1.0.0"
#endif

[Setup]
AppId={{8A6F2B1C-3D4E-4F5A-9B6C-7D8E9F0A1B2C}
AppName=ButterCams
AppVersion={#AppVersion}
AppPublisher=ButterCams
DefaultDirName={autopf}\ButterCams
DefaultGroupName=ButterCams
DisableProgramGroupPage=yes
DisableDirPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir=..\dist
OutputBaseFilename=ButterCams-{#AppVersion}-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayName=ButterCams Recorder
CloseApplications=no
SetupLogging=yes

[Languages]
Name: "en"; MessagesFile: "compiler:Default.isl"

[Files]
Source: "..\dist\buttercams.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\THIRD_PARTY_NOTICES.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\docs\OPERATIONS.md"; DestDir: "{app}\docs"; Flags: ignoreversion

[Icons]
Name: "{group}\ButterCams Settings"; Filename: "{app}\ButterCams Settings.url"

[UninstallDelete]
Type: files; Name: "{app}\ButterCams Settings.url"

[Run]
Filename: "{code:SettingsURL}"; Description: "Open the ButterCams settings page"; Flags: postinstall shellexec nowait skipifsilent

[Code]
const
  LineBreak = #13#10;
  DRIVE_REMOTE = 4;
  MinStagingGB = 100;
  DefaultPort = '8650';

var
  FoldersPage: TInputDirWizardPage;
  DestPage: TWizardPage;
  DestEnable: TNewCheckBox;
  DestLocal, DestShare: TNewRadioButton;
  DestPath, DestUser, DestPass, DestCap: TNewEdit;
  DestResult: TNewStaticText;
  WebPage: TWizardPage;
  PortEdit, AdminUser, AdminPass, AdminPass2: TNewEdit;
  AccountPage: TWizardPage;
  AccSystem, AccCustom: TNewRadioButton;
  AccName, AccPass: TNewEdit;
  Upgrade: Boolean;
  ExistingRoot: String;

function GetDriveType(lpRootPathName: String): UINT;
  external 'GetDriveTypeW@kernel32.dll stdcall';
function SetEnvironmentVariable(lpName, lpValue: String): BOOL;
  external 'SetEnvironmentVariableW@kernel32.dll stdcall';

function IsUNC(const Path: String): Boolean;
begin
  Result := Copy(Path, 1, 2) = '\\';
end;

function IsMappedDrive(const Path: String): Boolean;
begin
  Result := (Length(Path) >= 2) and (Path[2] = ':') and
    (GetDriveType(Copy(Path, 1, 2) + '\') = DRIVE_REMOTE);
end;

function Quote(const S: String): String;
begin
  Result := '"' + S + '"';
end;

function AddLabel(Page: TWizardPage; const Caption: String; Top: Integer): TNewStaticText;
begin
  Result := TNewStaticText.Create(Page);
  Result.Parent := Page.Surface;
  Result.Caption := Caption;
  Result.Top := Top;
  Result.Left := 0;
end;

function AddEdit(Page: TWizardPage; Top: Integer; Password: Boolean): TNewEdit;
begin
  Result := TNewEdit.Create(Page);
  Result.Parent := Page.Surface;
  Result.Top := Top;
  Result.Left := 0;
  Result.Width := Page.SurfaceWidth;
  if Password then
    Result.PasswordChar := '*';
end;

{ Runs buttercams.exe with the given arguments and returns its exit code;
  Output receives what it printed. Passwords travel in the environment. }
function RunButterCams(const Exe, Args: String; var Output: String): Integer;
var
  OutFile: String;
  Lines: AnsiString;
begin
  OutFile := ExpandConstant('{tmp}\buttercams-out.txt');
  DeleteFile(OutFile);
  Result := -1;
  if not Exec(ExpandConstant('{cmd}'), '/C "' + Quote(Exe) + ' ' + Args + ' > ' + Quote(OutFile) + ' 2>&1"',
    '', SW_HIDE, ewWaitUntilTerminated, Result) then
    Result := -1;
  Output := '';
  if LoadStringFromFile(OutFile, Lines) then
    Output := Trim(String(Lines));
  DeleteFile(OutFile);
end;

function DestArgs(): String;
begin
  if DestShare.Checked then
    Result := '--dest-type unc'
  else
    Result := '--dest-type local';
  Result := Result + ' --dest-path ' + Quote(Trim(DestPath.Text));
  if DestShare.Checked and (Trim(DestUser.Text) <> '') then
    Result := Result + ' --dest-user ' + Quote(Trim(DestUser.Text));
end;

procedure UpdateDestControls(Sender: TObject);
var
  OnOff, Share: Boolean;
begin
  OnOff := DestEnable.Checked;
  Share := OnOff and DestShare.Checked;
  DestLocal.Enabled := OnOff;
  DestShare.Enabled := OnOff;
  DestPath.Enabled := OnOff;
  DestCap.Enabled := OnOff;
  DestUser.Enabled := Share;
  DestPass.Enabled := Share;
end;

procedure TestDestinationClick(Sender: TObject);
var
  Exe, Output: String;
  Code: Integer;
begin
  if Trim(DestPath.Text) = '' then
  begin
    DestResult.Caption := 'Enter the destination path first.';
    exit;
  end;
  DestResult.Caption := 'Testing ...';
  WizardForm.Refresh;
  ExtractTemporaryFile('buttercams.exe');
  Exe := ExpandConstant('{tmp}\buttercams.exe');
  SetEnvironmentVariable('BUTTERCAMS_DEST_PASSWORD', DestPass.Text);
  Code := RunButterCams(Exe, 'setup test-destination ' + DestArgs(), Output);
  SetEnvironmentVariable('BUTTERCAMS_DEST_PASSWORD', '');
  if Code = 0 then
    DestResult.Caption := Output
  else
    DestResult.Caption := 'Failed: ' + Output;
end;

procedure InitializeWizard();
var
  Btn: TNewButton;
  Y: Integer;
begin
  Upgrade := RegQueryStringValue(HKLM64, 'SOFTWARE\ButterCams', 'DataRoot', ExistingRoot) and
    FileExists(AddBackslash(ExistingRoot) + 'config.json');

  { 1. Data root and staging folder }
  FoldersPage := CreateInputDirPage(wpWelcome, 'Where ButterCams keeps its data',
    'Choose the data folder and the staging folder.',
    'The data folder holds the settings, the index, logs and certificates.' + LineBreak +
    'Every camera records into the staging folder first; finished hours are then copied to your storage ' +
    'destinations. Staging must be on a local disk with plenty of room - ideally not the Windows drive.',
    False, '');
  FoldersPage.Add('Data folder:');
  FoldersPage.Add('Staging folder (leave empty to use "staging" inside the data folder):');
  FoldersPage.Values[0] := ExpandConstant('{commonappdata}\ButterCams');
  FoldersPage.Values[1] := '';

  { 2. First destination (optional) }
  DestPage := CreateCustomPage(FoldersPage.ID, 'Where recordings are stored',
    'Optionally add the first storage destination now. You can also do this later on the settings page.');
  DestEnable := TNewCheckBox.Create(DestPage);
  DestEnable.Parent := DestPage.Surface;
  DestEnable.Caption := 'Add a destination now';
  DestEnable.Width := DestPage.SurfaceWidth;
  DestEnable.OnClick := @UpdateDestControls;
  Y := DestEnable.Top + DestEnable.Height + ScaleY(6);
  DestLocal := TNewRadioButton.Create(DestPage);
  DestLocal.Parent := DestPage.Surface;
  DestLocal.Caption := 'Local or attached disk';
  DestLocal.Top := Y;
  DestLocal.Width := DestPage.SurfaceWidth div 2;
  DestLocal.Checked := True;
  DestLocal.OnClick := @UpdateDestControls;
  DestShare := TNewRadioButton.Create(DestPage);
  DestShare.Parent := DestPage.Surface;
  DestShare.Caption := 'Network share (\\server\share)';
  DestShare.Top := Y;
  DestShare.Left := DestPage.SurfaceWidth div 2;
  DestShare.Width := DestPage.SurfaceWidth div 2;
  DestShare.OnClick := @UpdateDestControls;
  Y := Y + DestLocal.Height + ScaleY(8);
  AddLabel(DestPage, 'Path, for example E:\Recordings or \\nas01\cameras:', Y);
  Y := Y + ScaleY(16);
  DestPath := AddEdit(DestPage, Y, False);
  Y := Y + DestPath.Height + ScaleY(6);
  AddLabel(DestPage, 'Storage limit in GB (the oldest recordings are deleted beyond it):', Y);
  Y := Y + ScaleY(16);
  DestCap := AddEdit(DestPage, Y, False);
  DestCap.Width := ScaleX(100);
  DestCap.Text := '1000';
  Y := Y + DestCap.Height + ScaleY(6);
  AddLabel(DestPage, 'Share user name and password (leave empty to use the service account):', Y);
  Y := Y + ScaleY(16);
  DestUser := AddEdit(DestPage, Y, False);
  DestUser.Width := DestPage.SurfaceWidth div 2 - ScaleX(4);
  DestPass := AddEdit(DestPage, Y, True);
  DestPass.Left := DestPage.SurfaceWidth div 2 + ScaleX(4);
  DestPass.Width := DestPage.SurfaceWidth div 2 - ScaleX(4);
  Y := Y + DestUser.Height + ScaleY(8);
  Btn := TNewButton.Create(DestPage);
  Btn.Parent := DestPage.Surface;
  Btn.Caption := 'Test';
  Btn.Top := Y;
  Btn.Width := ScaleX(75);
  Btn.Height := ScaleY(23);
  Btn.OnClick := @TestDestinationClick;
  DestResult := TNewStaticText.Create(DestPage);
  DestResult.Parent := DestPage.Surface;
  DestResult.Top := Y + ScaleY(4);
  DestResult.Left := Btn.Width + ScaleX(8);
  DestResult.Width := DestPage.SurfaceWidth - DestResult.Left;
  DestResult.AutoSize := False;
  DestResult.WordWrap := True;
  DestResult.Height := ScaleY(40);
  UpdateDestControls(nil);

  { 3. Web port and first administrator }
  WebPage := CreateCustomPage(DestPage.ID, 'Settings page',
    'Choose the port of the settings page and create the first administrator.');
  AddLabel(WebPage, 'Web port (the settings page will be at https://<this server>:<port>):', 0);
  PortEdit := AddEdit(WebPage, ScaleY(16), False);
  PortEdit.Width := ScaleX(100);
  PortEdit.Text := DefaultPort;
  AddLabel(WebPage, 'Administrator user name:', ScaleY(52));
  AdminUser := AddEdit(WebPage, ScaleY(68), False);
  AdminUser.Text := 'admin';
  AddLabel(WebPage, 'Password (at least 8 characters):', ScaleY(100));
  AdminPass := AddEdit(WebPage, ScaleY(116), True);
  AddLabel(WebPage, 'Repeat the password:', ScaleY(148));
  AdminPass2 := AddEdit(WebPage, ScaleY(164), True);

  { 4. Service account }
  AccountPage := CreateCustomPage(WebPage.ID, 'Service account',
    'Choose the Windows account the ButterCams service runs as.');
  AccSystem := TNewRadioButton.Create(AccountPage);
  AccSystem.Parent := AccountPage.Surface;
  AccSystem.Caption := 'Local System (default)';
  AccSystem.Width := AccountPage.SurfaceWidth;
  AccSystem.Checked := True;
  AccCustom := TNewRadioButton.Create(AccountPage);
  AccCustom.Parent := AccountPage.Surface;
  AccCustom.Caption := 'This account (recommended when writing to network shares without their own credentials):';
  AccCustom.Top := ScaleY(26);
  AccCustom.Width := AccountPage.SurfaceWidth;
  AddLabel(AccountPage, 'Account, for example CORP\svc-buttercams or .\buttercams:', ScaleY(56));
  AccName := AddEdit(AccountPage, ScaleY(72), False);
  AddLabel(AccountPage, 'Password:', ScaleY(104));
  AccPass := AddEdit(AccountPage, ScaleY(120), True);
  AddLabel(AccountPage, 'The account is given the "Log on as a service" right.', ScaleY(152));
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  { An upgrade keeps the existing settings, accounts, certificate and recordings. }
  Result := Upgrade and ((PageID = FoldersPage.ID) or (PageID = DestPage.ID) or (PageID = WebPage.ID));
end;

function DataRoot(): String;
begin
  if Upgrade then
    Result := ExistingRoot
  else
    Result := RemoveBackslashUnlessRoot(Trim(FoldersPage.Values[0]));
end;

function StagingPath(): String;
begin
  Result := RemoveBackslashUnlessRoot(Trim(FoldersPage.Values[1]));
  if Result = '' then
    Result := AddBackslash(DataRoot()) + 'staging';
end;

function PortValue(): String;
begin
  Result := Trim(PortEdit.Text);
end;

function SettingsURL(Param: String): String;
var
  Url: String;
begin
  Url := GetIniString('InternetShortcut', 'URL', '', ExpandConstant('{app}\ButterCams Settings.url'));
  if Url = '' then
    Url := 'https://localhost:' + PortValue();
  Result := Url;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  FreeBytes, TotalBytes: Int64;
  Port, Cap: Integer;
  Staging: String;
begin
  Result := True;
  if CurPageID = FoldersPage.ID then
  begin
    Staging := StagingPath();
    if (DataRoot() = '') or IsUNC(DataRoot()) then
    begin
      MsgBox('Choose a data folder on a local disk.', mbError, MB_OK);
      Result := False;
      exit;
    end;
    if IsUNC(Staging) or IsMappedDrive(Staging) then
    begin
      MsgBox('Staging must be on a local disk, not a network path or mapped drive.', mbError, MB_OK);
      Result := False;
      exit;
    end;
    if GetSpaceOnDisk64(ExtractFileDrive(Staging) + '\', FreeBytes, TotalBytes) then
      if FreeBytes div (1024 * 1024 * 1024) < MinStagingGB then
        if MsgBox('The staging drive has less than 100 GB free (' + IntToStr(FreeBytes div (1024 * 1024 * 1024)) +
          ' GB). ButterCams deletes the oldest staging files when space runs low, so recordings could be lost ' +
          'before they are copied.' + LineBreak + LineBreak + 'Use this folder anyway?', mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDNO then
        begin
          Result := False;
          exit;
        end;
    if CompareText(ExtractFileDrive(Staging), ExpandConstant('{sd}')) = 0 then
      if MsgBox('The staging folder is on the Windows drive. A dedicated disk is safer: recording writes ' +
        'continuously and Windows needs free space too.' + LineBreak + LineBreak + 'Use this folder anyway?',
        mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDNO then
        Result := False;
  end
  else if (CurPageID = DestPage.ID) and DestEnable.Checked then
  begin
    Cap := StrToIntDef(Trim(DestCap.Text), 0);
    if Trim(DestPath.Text) = '' then
    begin
      MsgBox('Enter the destination path, or untick "Add a destination now".', mbError, MB_OK);
      Result := False;
    end
    else if DestShare.Checked and not IsUNC(Trim(DestPath.Text)) then
    begin
      MsgBox('Enter the share as \\server\share\folder.', mbError, MB_OK);
      Result := False;
    end
    else if DestLocal.Checked and IsMappedDrive(Trim(DestPath.Text)) then
    begin
      MsgBox('Mapped drives aren''t visible to Windows services. Use the \\server\share path instead.', mbError, MB_OK);
      Result := False;
    end
    else if Cap <= 0 then
    begin
      MsgBox('Enter the storage limit in GB (a whole number greater than 0).', mbError, MB_OK);
      Result := False;
    end;
  end
  else if CurPageID = WebPage.ID then
  begin
    Port := StrToIntDef(PortValue(), 0);
    if (Port < 1) or (Port > 65535) then
    begin
      MsgBox('Port must be a number between 1 and 65535.', mbError, MB_OK);
      Result := False;
    end
    else if Trim(AdminUser.Text) = '' then
    begin
      MsgBox('Enter a user name for the administrator.', mbError, MB_OK);
      Result := False;
    end
    else if Length(AdminPass.Text) < 8 then
    begin
      MsgBox('The password needs at least 8 characters.', mbError, MB_OK);
      Result := False;
    end
    else if AdminPass.Text <> AdminPass2.Text then
    begin
      MsgBox('The two passwords are not the same.', mbError, MB_OK);
      Result := False;
    end;
  end
  else if (CurPageID = AccountPage.ID) and AccCustom.Checked then
  begin
    if Trim(AccName.Text) = '' then
    begin
      MsgBox('Enter the account name, or choose Local System.', mbError, MB_OK);
      Result := False;
    end;
  end;
end;

{ Stop a running service before its executable is replaced (upgrade). }
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
begin
  Result := '';
  Exec(ExpandConstant('{sys}\net.exe'), 'stop ButterCams', '', SW_HIDE, ewWaitUntilTerminated, Code);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Exe, Args, Output: String;
  Code: Integer;
begin
  if CurStep <> ssPostInstall then
    exit;
  Exe := ExpandConstant('{app}\buttercams.exe');

  { Data root, configuration, first administrator, certificate. On an upgrade
    this keeps everything that exists. }
  Args := 'setup init --data-root ' + Quote(DataRoot());
  if not Upgrade then
  begin
    Args := Args + ' --staging ' + Quote(StagingPath()) + ' --port ' + PortValue() +
      ' --admin-user ' + Quote(Trim(AdminUser.Text));
    SetEnvironmentVariable('BUTTERCAMS_ADMIN_PASSWORD', AdminPass.Text);
    if DestEnable.Checked then
    begin
      Args := Args + ' ' + DestArgs() + ' --dest-cap-gb ' + Trim(DestCap.Text);
      SetEnvironmentVariable('BUTTERCAMS_DEST_PASSWORD', DestPass.Text);
    end;
  end;
  Code := RunButterCams(Exe, Args, Output);
  SetEnvironmentVariable('BUTTERCAMS_ADMIN_PASSWORD', '');
  SetEnvironmentVariable('BUTTERCAMS_DEST_PASSWORD', '');
  if Code <> 0 then
    RaiseException('ButterCams could not prepare its data folder:' + LineBreak + Output);

  if not Upgrade then
    SetIniString('InternetShortcut', 'URL', 'https://localhost:' + PortValue(),
      ExpandConstant('{app}\ButterCams Settings.url'));

  { Service: Automatic (Delayed Start), restart after 5 s on every failure,
    firewall rule for Domain and Private networks, then start. }
  Args := 'setup install-service --data-root ' + Quote(DataRoot());
  if AccCustom.Checked then
  begin
    Args := Args + ' --account ' + Quote(Trim(AccName.Text));
    SetEnvironmentVariable('BUTTERCAMS_SERVICE_PASSWORD', AccPass.Text);
  end;
  Code := RunButterCams(Exe, Args, Output);
  SetEnvironmentVariable('BUTTERCAMS_SERVICE_PASSWORD', '');
  if Code <> 0 then
    RaiseException('The ButterCams service could not be installed:' + LineBreak + Output);
end;

{ Uninstall: recordings on destinations are never touched. The data folder
  (settings, index, logs, recordings still in staging) is only removed when
  the user explicitly says so. }
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Args: String;
  Code: Integer;
begin
  if CurUninstallStep <> usUninstall then
    exit;
  Args := 'setup uninstall-service';
  if not UninstallSilent() then
    if MsgBox('Also remove the ButterCams data folder (settings, index, logs and any recordings still in staging)?' + LineBreak + LineBreak +
      'Recordings on your storage destinations are never deleted.' + LineBreak +
      'Choose No to keep everything for a later reinstall.', mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES then
      Args := Args + ' --remove-data';
  Exec(ExpandConstant('{app}\buttercams.exe'), Args, '', SW_HIDE, ewWaitUntilTerminated, Code);
end;
