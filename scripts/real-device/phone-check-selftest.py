#!/usr/bin/env python3
"""Day 33. Rehearse the phone check in a desktop browser before you carry it to a phone.

    OFFGRID_BIN=./bin/offgrid python3 scripts/real-device/phone-check-selftest.py

What it does (headless Chromium via Playwright, `pip install playwright && playwright install chromium`):
  1. makes a CA and a node certificate for 127.0.0.1 with the real `offgrid ca` commands
  2. starts `offgrid ca spike`, opens the page, waits for every check
  3. reloads (so the service worker controls the page), stops the node AND cuts the browser's network,
     reloads again: the page must say OFFLINE LOAD, served by the service worker
  4. restarts the node, goes online, reloads: the page must send its offline evidence by itself
  5. reads the node's results file and checks it recorded the offline load

This proves the PAGE and the service worker logic. It does not prove a phone: Chromium here trusts the
certificate with --ignore-certificate-errors, and 127.0.0.1 is always a "secure context", so the certificate
authority install, which is the whole point of the phone test, is NOT exercised. Only a real phone does that.
Exit 0 means every step held.
"""
import json
import os
import subprocess
import sys
import tempfile
import time

try:
    from playwright.sync_api import sync_playwright
except ImportError:
    sys.exit("needs Playwright: pip install playwright && playwright install chromium")

BIN = os.path.abspath(os.environ.get("OFFGRID_BIN", "bin/offgrid"))
if not os.access(BIN, os.X_OK):
    sys.exit(f"no binary at {BIN} (run: make build, or set OFFGRID_BIN)")

tmp = tempfile.mkdtemp(prefix="phone-check-")
ca, node, results = f"{tmp}/ca", f"{tmp}/node", f"{tmp}/results.jsonl"
for cmd in (["ca", "init", "-dir", ca], ["ca", "issue", "-dir", ca, "-out", node, "-name", "127.0.0.1"]):
    subprocess.run([BIN, *cmd], check=True, stdout=subprocess.DEVNULL)

def start():
    p = subprocess.Popen([BIN, "ca", "spike", "-cert", f"{node}/node.crt", "-key", f"{node}/node.key", "-ca", f"{ca}/ca.crt",
                          "-addr", "127.0.0.1:18443", "-http-addr", "", "-results", results, "-for", "5m"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1.0)
    return p

def until(pred, what, secs=20):
    end = time.time() + secs
    while time.time() < end:
        if pred():
            return
        time.sleep(0.2)
    raise SystemExit(f"FAIL: timed out waiting for {what}")

failures = []
def check(ok, what):
    print(("ok    " if ok else "FAIL  ") + what)
    if not ok:
        failures.append(what)

srv = start()
try:
    with sync_playwright() as p:
        b = p.chromium.launch(args=["--ignore-certificate-errors"])
        ctx = b.new_context(ignore_https_errors=True)
        page = ctx.new_page()
        page.goto("https://127.0.0.1:18443/")
        until(lambda: page.locator("#checks li").count() >= 9, "the checks to finish")
        time.sleep(1)
        rows = page.eval_on_selector_all("#checks li", "els => els.map(e => e.innerText)")
        check(not any(r.startswith("FAIL") for r in rows), f"online: no failed check ({len(rows)} checks)")
        check(any("Device key survives" in r and r.startswith("PASS") for r in rows), "online: a device key survives storage in IndexedDB")
        page.reload(); time.sleep(1.5)
        srv.terminate(); srv.wait(); ctx.set_offline(True)
        page.reload()
        until(lambda: "OFFLINE LOAD" in page.inner_text("#offline"), "an offline load")
        check("served by the service worker" in page.inner_text("#offline"), "offline: the page loaded from the service worker with the node gone")
        srv = start(); ctx.set_offline(False)
        page.reload()
        until(lambda: "Sent automatically" in page.inner_text("#sendstate"), "the automatic send")
        check(True, "reconnected: the page sent its result by itself")
        b.close()
finally:
    srv.terminate()

lines = [json.loads(l) for l in open(results)] if os.path.exists(results) else []
check(len(lines) >= 2, f"the node stored {len(lines)} result(s)")
check(any(x.get("controlled") for l in lines for x in l.get("offline_loads", [])), "the node's record includes the offline load")
print("\nRESULT:", "FAIL" if failures else "PASS")
sys.exit(1 if failures else 0)
