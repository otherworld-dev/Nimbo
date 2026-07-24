# Managed deployment — admin policy

Lock down Nimbo across a fleet. Policies live in the registry under
**`HKLM\SOFTWARE\Policies\Nimbo`** and **override** the user's own settings. They
apply only on machines with a **Nimbo business licence** installed (policy is a
business-tier capability) — see `tools/licensegen/README.md`.

Set them with Group Policy / Intune via the bundled ADMX template, or write the
registry values directly (MDM, a deployment script, an MSI transform).

## Available policies

| Registry value (REG type) | Effect |
|---|---|
| `ServerURL` (SZ) | Preset the Nextcloud server users sign in to |
| `LockServer` (DWORD 1) | Users can't change the server — sign-in is forced to `ServerURL` (stops connecting a personal Nextcloud) |
| `AllowSignOut` (DWORD 0) | Hides/blocks Sign out (managed account can't be disconnected) |
| `LockBandwidth` (DWORD 1) + `UploadKBps`/`DownloadKBps` (DWORD) | Enforce bandwidth limits; users can't change them (0 = unlimited) |
| `SyncMode` (SZ `live`\|`ondemand`) | Force the file-availability mode; users can't change it |

The app shows a "Some settings are managed by your organisation" banner and
locks the affected controls. Any value you don't set stays user-controlled.

## Group Policy / Intune (ADMX)

1. Copy `Nimbo.admx` to `C:\Windows\PolicyDefinitions\` and
   `en-US\Nimbo.adml` to `C:\Windows\PolicyDefinitions\en-US\` — or into your
   domain **Central Store** (`\\<domain>\SYSVOL\<domain>\Policies\PolicyDefinitions`).
2. In the Group Policy editor: **Computer Configuration → Administrative
   Templates → Nimbo**. Configure the policies and link the GPO to the target
   OU. (Intune: import the ADMX/ADML as a custom administrative template.)

## Direct registry (scripted deployment)

```powershell
$k = 'HKLM:\SOFTWARE\Policies\Nimbo'
New-Item -Path $k -Force | Out-Null
Set-ItemProperty $k -Name ServerURL     -Value 'https://cloud.example.com' -Type String
Set-ItemProperty $k -Name LockServer    -Value 1 -Type DWord
Set-ItemProperty $k -Name AllowSignOut  -Value 0 -Type DWord
Set-ItemProperty $k -Name LockBandwidth -Value 1 -Type DWord
Set-ItemProperty $k -Name UploadKBps    -Value 2000 -Type DWord
Set-ItemProperty $k -Name DownloadKBps  -Value 0 -Type DWord
Set-ItemProperty $k -Name SyncMode      -Value 'ondemand' -Type String
```

Policies are read at launch (and when a licence is activated). Restart Nimbo, or
re-run Group Policy (`gpupdate /force`) and restart, to apply changes.

## Notes

- Without a business licence these keys are ignored — the install behaves as a
  normal free personal copy.
- The ADMX namespace/registry path is the stock-brand `Nimbo`. A white-label
  build that needs its own policy path changes `policyKey` in
  `internal/policy/policy_windows.go` and the ADMX `key`/namespace to match.
