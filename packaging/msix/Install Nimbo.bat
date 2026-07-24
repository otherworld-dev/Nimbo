@echo off
REM Double-click to install Nimbo. Runs the PowerShell installer (which will
REM prompt for administrator rights to trust the certificate and install).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0Install-Nimbo.ps1"
