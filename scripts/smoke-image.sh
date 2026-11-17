#!/usr/bin/env bash
set -euo pipefail
image="${1:-go-bot/host:local}"
docker run --rm --entrypoint /bin/sh "$image" -ec '
  python --version
  python -m pip --version
  node --version
  npm --version
  npx --version
  chromium --version
  soffice --version
  officecli --version
  python -c "import pypdf,pdfplumber,reportlab,pymupdf,pypdfium2,pptx"
  mkdir /tmp/office-smoke
  officecli create /tmp/office-smoke/report.docx
  officecli add /tmp/office-smoke/report.docx /body --type paragraph --prop "text=Go Bot smoke"
  officecli get /tmp/office-smoke/report.docx /
  officecli save /tmp/office-smoke/report.docx
  soffice -env:UserInstallation=file:///tmp/lo-smoke --headless --convert-to pdf --outdir /tmp/office-smoke /tmp/office-smoke/report.docx
  pdfinfo /tmp/office-smoke/report.pdf
  chromium --headless --no-sandbox --disable-dev-shm-usage --no-zygote --screenshot=/tmp/browser.png "data:text/html,<h1>Go Bot</h1>" >/dev/null 2>&1
  test -s /tmp/browser.png
  agent-browser --version
  agent-browser skills get core > /tmp/agent-browser-core.md
  grep -q "agent-browser core" /tmp/agent-browser-core.md
  export AGENT_BROWSER_SOCKET_DIR="$(mktemp -d /tmp/ghb-XXXXXXXX)"
  trap "agent-browser close --all" EXIT
  agent-browser open about:blank
  echo "Image document/browser smoke: OK"
'
