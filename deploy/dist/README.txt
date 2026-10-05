spendbot — spending tracker for Kaspi Gold statements with a local model
=======================================================================

Everything runs on your computer: your data and the model never leave it.

STARTING
--------
Windows:
  1. Unzip the archive into a regular folder, such as Documents\spendbot
     (not Program Files — the program keeps its data next to itself).
  2. Double-click start.bat.
     Windows may say "Windows protected your PC" because the program is not signed.
     Click "More info" → "Run anyway".
  3. start.bat offers to install Ollama, the free app that runs the local model.
     Say yes — it needs an internet connection.
  4. A page opens in your browser. If it does not, open http://127.0.0.1:8080
  Keep the black window open while you use the program. Closing it stops the program;
  run start.bat again to start it.

macOS:
  Double-click start.command. If macOS refuses to open it, right-click → Open.
  Ollama: https://ollama.com/download

Linux:
  Run ./spendbot in the program folder. Ollama: curl -fsSL https://ollama.com/install.sh | sh

FIRST STEPS
-----------
  1. Settings → pick a model for your computer and click "Download and use":
       qwen2.5:7b — 4.7 GB, with 16 GB of RAM or more (best);
       qwen2.5:3b — 1.9 GB, with 8 GB of RAM;
       qwen2.5:1.5b — 1 GB, for a weak computer.
     The download takes a few minutes; progress is shown on the page.
  2. Download a statement in the Kaspi app: Kaspi Gold → Statement → up to one year → PDF.
     On the home page click "Choose PDF". You can upload several statements for different
     years — there will be no duplicates.
  3. The model goes through the merchants and builds batches of "what is this shop"
     questions. Answer them on the home page to categorize your spending.
  4. Analytics shows spending by month, weekday and category, and the top merchants.
     The "Reconciled with statement" block shows that the sums match the bank to the tiyn.
     Transfers shows how much was sent to and received from each person.

DATA
----
  Everything is stored in the data folder next to the program; backups go to
  data\backups (every night while the program runs). To move to another computer or make
  a copy, copy the whole folder while the program is closed.

SETTINGS
--------
  The spendbot.env file (opens in Notepad). A Telegram bot can be connected there too.
  If port 8080 is busy, change 8080 to another port in LISTEN_ADDR and PUBLIC_URL.
