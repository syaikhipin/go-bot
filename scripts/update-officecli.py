#!/usr/bin/env python3
"""Update official OfficeCLI version/checksum locks; rebuild and smoke afterwards."""
import json
import pathlib
import re
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent


def fetch(url):
    request = urllib.request.Request(url, headers={"User-Agent": "Go-Bot-build"})
    with urllib.request.urlopen(request, timeout=60) as response:
        data = response.read(2_000_001)
    if len(data) > 2_000_000:
        raise RuntimeError("Official release metadata exceeded expected size")
    return data


def main():
    release = json.loads(fetch("https://api.github.com/repos/iOfficeAI/OfficeCLI/releases/latest"))
    version = release["tag_name"]
    if release.get("prerelease") or release.get("draft") or not re.fullmatch(r"v\d+\.\d+\.\d+", version):
        raise RuntimeError("Expected a stable official release")
    checksums = fetch(f"https://github.com/iOfficeAI/OfficeCLI/releases/download/{version}/SHA256SUMS").decode()
    wanted = ("officecli-linux-arm64", "officecli-linux-x64")
    found = {}
    for line in checksums.splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64})\s+\*?(\S+)", line)
        if match and match[2] in wanted:
            found[match[2]] = match[1].lower()
    if set(found) != set(wanted):
        raise RuntimeError("Official SHA256SUMS is missing required Linux assets")
    dockerfile = ROOT / "build/Dockerfile.host"
    updated, count = re.subn(r"ARG OFFICECLI_VERSION=v\d+\.\d+\.\d+", f"ARG OFFICECLI_VERSION={version}", dockerfile.read_text())
    if count != 1:
        raise RuntimeError("Expected exactly one OfficeCLI build version")
    # All metadata is validated before modifying tracked files.
    (ROOT / "build/officecli.sha256").write_text("".join(f"{found[name]}  {name}\n" for name in wanted))
    (ROOT / "build/dependencies.lock.yaml").write_text(f"officecli:\n  repository: iOfficeAI/OfficeCLI\n  version: {version}\n  checksums: build/officecli.sha256\n")
    dockerfile.write_text(updated)
    print(f"Locked official OfficeCLI {version}; review changes, rebuild and smoke before publishing.")


if __name__ == "__main__":
    main()
