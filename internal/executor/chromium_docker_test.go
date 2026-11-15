package executor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Exercise screenshot, PDF and download with the Host launcher's default
// Docker isolation. The image runs as root and passes --no-sandbox to Chromium.
func TestFormalDockerRootChromiumRenderAndDownload(t *testing.T) {
	image := os.Getenv("SERVERAGENTS_HOST_FORMAL_IMAGE")
	if image == "" {
		t.Skip("set SERVERAGENTS_HOST_FORMAL_IMAGE to the complete Host image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	page := "data:text/html,%3Chtml%3E%3Cbody%3E%3Ch1%3EHarness%3C%2Fh1%3E%3C%2Fbody%3E%3C%2Fhtml%3E"
	script := `set -eu
touch /tmp/host-ownership-check
chown 12345:12345 /tmp/host-ownership-check
test "$(stat -c %u:%g /tmp/host-ownership-check)" = 12345:12345
chromium --headless --no-sandbox --disable-gpu --no-first-run --no-default-browser-check --user-data-dir=/tmp/chromium-shot --screenshot=/root/harness/page.png --window-size=800,600 "$1" >/tmp/chromium-shot.log 2>&1 || { tail -30 /tmp/chromium-shot.log; exit 1; }
test -s /root/harness/page.png
chromium --headless --no-sandbox --disable-gpu --no-first-run --no-default-browser-check --user-data-dir=/tmp/chromium-pdf --print-to-pdf=/root/harness/page.pdf "$1" >/tmp/chromium-pdf.log 2>&1 || { tail -30 /tmp/chromium-pdf.log; exit 1; }
test -s /root/harness/page.pdf
mkdir /root/harness/downloads
python3 -u - >/tmp/download-http.log 2>&1 <<'PY' &
from http.server import HTTPServer, BaseHTTPRequestHandler

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Disposition", 'attachment; filename="download.txt"')
        self.end_headers()
        self.wfile.write(b"harness-download-ok")

HTTPServer(("127.0.0.1", 18765), Handler).serve_forever()
PY
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT
curl --retry 5 --retry-connrefused --retry-delay 1 --silent --show-error http://127.0.0.1:18765/ >/dev/null
chromium --headless --no-sandbox --disable-gpu --no-first-run --no-default-browser-check --user-data-dir=/tmp/chromium-download --remote-debugging-port=18766 about:blank >/tmp/chromium-download.log 2>&1 &
browser=$!
python3 - <<'PY'
import base64
import json
import os
import socket
import struct
import time
import urllib.request

for attempt in range(30):
    try:
        with urllib.request.urlopen("http://127.0.0.1:18766/json", timeout=1) as response:
            pages = json.load(response)
        page = next(item for item in pages if item.get("type") == "page")
        break
    except (OSError, StopIteration):
        time.sleep(0.1)
else:
    raise RuntimeError("Chromium debugging endpoint did not open")

url = page["webSocketDebuggerUrl"]
path = url.split("18766", 1)[1]
key = base64.b64encode(os.urandom(16)).decode()
connection = socket.create_connection(("127.0.0.1", 18766), timeout=5)
connection.sendall((f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1:18766\r\n"
                    f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
                    f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
response = b""
while b"\r\n\r\n" not in response:
    response += connection.recv(4096)
if b" 101 " not in response.split(b"\r\n", 1)[0]:
    raise RuntimeError(f"Chromium rejected WebSocket: {response[:200]!r}")

def send(command_id, method, params):
    payload = json.dumps({"id": command_id, "method": method, "params": params}).encode()
    mask = os.urandom(4)
    size = len(payload)
    length = bytes([0x80 | size]) if size < 126 else bytes([0x80 | 126]) + struct.pack("!H", size)
    connection.sendall(b"\x81" + length + mask + bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload)))

send(1, "Page.setDownloadBehavior", {"behavior": "allow", "downloadPath": "/root/harness/downloads"})
send(2, "Page.navigate", {"url": "http://127.0.0.1:18765/file"})
for attempt in range(100):
    try:
        with open("/root/harness/downloads/download.txt", "rb") as downloaded:
            if downloaded.read() == b"harness-download-ok":
                break
    except FileNotFoundError:
        pass
    time.sleep(0.1)
else:
    raise RuntimeError("Chromium did not download the attachment")
connection.close()
PY
kill "$browser" 2>/dev/null || true
test "$(cat /root/harness/downloads/download.txt 2>/dev/null)" = harness-download-ok || { tail -30 /tmp/chromium-download.log; exit 1; }
echo chromium-render-and-download-ok`
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--shm-size", "512m", "--entrypoint", "/bin/sh", image, "-c", script, "browser-smoke", page)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "chromium-render-and-download-ok") {
		t.Fatalf("sandboxed Chromium screenshot/PDF/download failed: %v\n%s", err, output)
	}
}
