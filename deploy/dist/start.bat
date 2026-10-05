@echo off
cd /d "%~dp0"
title spendbot

rem The local model needs Ollama. If it is missing, offer to install it with winget.
where ollama >nul 2>nul
if not errorlevel 1 goto run
if exist "%LOCALAPPDATA%\Programs\Ollama\ollama.exe" goto run

echo Ollama was not found. It is a free app that runs the local model.
echo spendbot works without it, but without category guesses and tips.
choice /c YN /m "Install Ollama now with winget"
if errorlevel 2 goto run
winget install --id Ollama.Ollama -e --accept-package-agreements --accept-source-agreements
if exist "%LOCALAPPDATA%\Programs\Ollama\ollama app.exe" start "" "%LOCALAPPDATA%\Programs\Ollama\ollama app.exe"
echo.
echo Done. Pick and download a model on the Settings page.
echo.

:run
spendbot.exe
