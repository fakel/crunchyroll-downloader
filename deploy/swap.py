"""Swap the server's binary: pause the queue so no new run starts, install the
new binary, restart, then re-queue anything the restart interrupted.

Run this on the server, after uploading the new build to
/root/crdl-install/crunchyroll-downloader.next:

    CRDL_WEB_USER=<user> CRDL_WEB_PASSWORD=<pass> python3 /root/crdl-install/swap.py

The web password is read from the environment, never hardcoded here. The
queue is paused first so no job starts mid-swap; a job that was running when
the service stops comes back as "interrupted" and is re-queued, which resumes
it from the episodes already on disk instead of starting over.
"""
import base64
import json
import os
import subprocess
import sys
import time
import urllib.request

URL = os.environ.get("CRDL_WEB_URL", "http://127.0.0.1:8080")
NEXT = "/root/crdl-install/crunchyroll-downloader.next"
DEST = "/opt/crunchyroll-downloader/crunchyroll-downloader"

user = os.environ.get("CRDL_WEB_USER", "")
password = os.environ.get("CRDL_WEB_PASSWORD", "")
if not user or not password:
    sys.exit("set CRDL_WEB_USER and CRDL_WEB_PASSWORD (the web basic-auth credentials)")
AUTH = {"Authorization": "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()}


def req(path, data=None, method="GET"):
    body = json.dumps(data).encode() if data is not None else None
    r = urllib.request.Request(URL + path, data=body, headers=AUTH, method=method)
    with urllib.request.urlopen(r, timeout=30) as resp:
        return json.loads(resp.read() or b"null")


print("pausing the queue", flush=True)
req("/api/pause", {"paused": True}, "POST")

subprocess.run(["install", "-m", "0755", NEXT, DEST], check=True)
subprocess.run(["systemctl", "restart", "crld-web"], check=True)
time.sleep(4)

state = req("/api/state")
requeued = []
for entry in state["series"]:
    job = entry.get("job")
    if not job or job["status"] != "interrupted":
        continue
    req("/api/jobs/%s/retry" % job["id"], {}, "POST")
    requeued.append(entry["title"])
print("requeued:", requeued, flush=True)

req("/api/pause", {"paused": False}, "POST")
print("swapped at", time.strftime("%Y-%m-%dT%H:%M:%S"), flush=True)
