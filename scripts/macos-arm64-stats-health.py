#!/usr/bin/env python3
"""Compare native macOS ARM64 Engram stats opens using disposable SQLite stores."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import sqlite3
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request


MAIN = "f8fa1b460084dd5988c7e05cc72dec03a730f90f"
RELEASE = "v3.4.0"
REPOSITORY = "https://github.com/Gentleman-Programming/engram"
ASSET = "engram_3.4.0_darwin_arm64.tar.gz"
ASSET_SHA256 = "d0aa59c9a83d17ae3fc7f5dd31fdab6a26fd8c00fc5e692195753cd0719294f7"
PERIOD = 0.25
STAMP = "2026-01-01 00:00:00"
HEALTH_TIMEOUT = 30
STATS_TIMEOUT = 120
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def run(args, *, timeout=30, **kwargs):
    result = subprocess.run(args, text=True, capture_output=True, timeout=timeout, **kwargs)
    if result.returncode:
        raise RuntimeError(f"{args[0]} exited {result.returncode}: {result.stderr[-4000:]}")
    return result.stdout.strip()


def seed(db, observations=13_000, prompts=30_000, journal=83_000, pending=7_700):
    projects = [f"fixture-{index:02d}" for index in range(26)]
    sessions = [(f"s-{index}", projects[index % len(projects)]) for index in range(312)]
    content = ("engram synthetic orchard signal and concurrency checkpoint evidence " * 140)[:8_600]

    def journal_rows():
        for index in range(journal):
            row = index % observations
            session, project = sessions[row % len(sessions)]
            observation = f"o-{row}"
            payload = {"sync_id": observation, "session_id": session, "title": f"Orchard {row}",
                       "content": content, "type": "decision", "scope": "project", "project": project}
            acked = STAMP if index < journal - pending else None
            yield observation, json.dumps(payload, separators=(",", ":")), project, STAMP, acked

    with closing(sqlite3.connect(db)) as conn:
        conn.execute("PRAGMA foreign_keys=ON")
        conn.execute("PRAGMA wal_autocheckpoint=0")
        conn.executemany("INSERT INTO sessions(id,project,directory,started_at) VALUES (?,?,?,?)",
                         ((session, project, f"/fixture/{project}", STAMP) for session, project in sessions))
        conn.executemany("INSERT INTO sync_enrolled_projects(project,enrolled_at) VALUES (?,?)",
                         ((project, STAMP) for project in projects))
        conn.executemany("""INSERT INTO observations(sync_id,session_id,type,title,content,project,created_at,updated_at)
                            VALUES (?,?,'decision',?,?,?,?,?)""",
                         ((f"o-{index}", session, f"Orchard {index}", content, project, STAMP, STAMP)
                          for index in range(observations) for session, project in (sessions[index % len(sessions)],)))
        conn.executemany("""INSERT INTO user_prompts(sync_id,session_id,content,project,created_at)
                            VALUES (?,?,?,?,?)""",
                         ((f"p-{index}", session, content, project, STAMP)
                          for index in range(prompts) for session, project in (sessions[index % len(sessions)],)))
        conn.execute("INSERT INTO sync_state(target_key,last_enqueued_seq,last_acked_seq) VALUES ('local:fixture',?,?)",
                     (journal, journal - pending))
        conn.executemany("""INSERT INTO sync_mutations(target_key,entity,entity_key,op,payload,project,occurred_at,acked_at)
                            VALUES ('local:fixture','observation',?,'upsert',?,?,?,?)""", journal_rows())
        conn.commit()
        if conn.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone() != (0, 0, 0):
            raise RuntimeError("fixture WAL checkpoint did not truncate")
        if conn.execute("PRAGMA quick_check").fetchone() != ("ok",):
            raise RuntimeError("fixture SQLite quick_check failed")
    return {"db_bytes": db.stat().st_size, "sessions": len(sessions), "observations": observations,
            "prompts": prompts, "journal_rows": journal, "unacknowledged": pending, "enrolled_projects": len(projects),
            "sample_content_bytes": len(content.encode())}


def query_health(url, epoch):
    start = time.monotonic()
    result = {"offset_s": round(start - epoch, 6)}
    try:
        with HTTP.open(url, timeout=HEALTH_TIMEOUT) as response:
            result["status"] = response.status
            response.read()
    except urllib.error.HTTPError as error:
        result["status"] = error.code
        error.close()
    except (OSError, urllib.error.URLError) as error:
        result["error"] = str(error)
    result["duration_s"] = round(time.monotonic() - start, 6)
    return result


def trial(clients, executable, directory, env, health_url):
    finished = threading.Event()
    epoch = 0.0

    def start_clock():
        nonlocal epoch
        epoch = time.monotonic()

    gate = threading.Barrier(clients + 1, action=start_clock, timeout=20)

    def stats():
        gate.wait()
        start = time.monotonic()
        run([executable, "stats"], cwd=directory, env=env, timeout=STATS_TIMEOUT)
        return {"started_s": round(start - epoch, 6), "elapsed_s": round(time.monotonic() - start, 6)}

    def sample():
        gate.wait()
        results = []
        while True:
            was_finished = finished.is_set()
            results.append(query_health(health_url, epoch))
            if was_finished:
                return results
            next_sample = epoch + (int((time.monotonic() - epoch) / PERIOD) + 1) * PERIOD
            finished.wait(max(0, next_sample - time.monotonic()))

    with ThreadPoolExecutor(max_workers=clients + 1) as pool:
        probes = pool.submit(sample)
        processes = [pool.submit(stats) for _ in range(clients)]
        try:
            elapsed = [worker.result() for worker in processes]
        finally:
            finished.set()
        samples = probes.result()
    last_stats = round(max(row["started_s"] + row["elapsed_s"] for row in elapsed), 6)
    during_stats = [probe["duration_s"] for probe in samples if probe["offset_s"] < last_stats]
    return {"stats_processes": clients, "processes": elapsed, "last_stats_s": last_stats,
            "peak_health_while_stats_s": max(during_stats, default=None), "health": samples}


def measure(root, executable, label, rounds, **seed_options):
    work = root / label
    work.mkdir()
    home, temporary, data = (work / name for name in ("home", "tmp", "data"))
    for path in (home, temporary, data):
        path.mkdir()
    env = {key: value for key, value in os.environ.items()
           if key.upper() not in {"HOME", "TMPDIR", "TMP", "TEMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}
           and not key.upper().startswith(("ENGRAM_", "GENTLE_", "XDG_"))}
    env.update(HOME=str(home), TMPDIR=str(temporary), TMP=str(temporary), TEMP=str(temporary),
               USERPROFILE=str(home), APPDATA=str(home), LOCALAPPDATA=str(home),
               ENGRAM_DATA_DIR=str(data), ENGRAM_CLOUD_AUTOSYNC="0", ENGRAM_NO_UPDATE_CHECK="1")
    db = data / "engram.db"
    with closing(sqlite3.connect(db)) as conn:
        conn.execute("PRAGMA user_version=0")
    run([executable, "stats"], cwd=work, env=env)
    fixture = seed(db, **seed_options)
    print(json.dumps({"version": label, "fixture": fixture}), flush=True)
    with socket.socket() as port_reservation:
        port_reservation.bind(("127.0.0.1", 0))
        port = port_reservation.getsockname()[1]
    health_url = f"http://127.0.0.1:{port}/health"
    log = work / "serve.log"
    with log.open("wb") as output, subprocess.Popen([executable, "serve", str(port)], cwd=work, env=env,
                                                    stdin=subprocess.DEVNULL, stdout=output, stderr=output) as server:
        try:
            deadline = time.monotonic() + 60
            while server.poll() is None and time.monotonic() < deadline:
                if query_health(health_url, time.monotonic()).get("status") == 200 and server.poll() is None:
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError(f"fixture serve could not initialize: {log.read_text(errors='replace')[-4000:]}")
            idle_epoch = time.monotonic()
            idle_health = []
            for sample_index in range(4):
                time.sleep(max(0, idle_epoch + sample_index * PERIOD - time.monotonic()))
                idle_health.append(query_health(health_url, idle_epoch))
            trials = [trial(clients, executable, work, env, health_url)
                      for _ in range(rounds) for clients in (1, 3)]
            return {"version": label, "fixture": fixture, "idle_health": idle_health, "trials": trials}
        finally:
            if server.poll() is None:
                server.terminate()
            try:
                server.wait(timeout=8)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait(timeout=8)


def extract_release(archive, destination):
    with archive.open("rb") as contents:
        actual = hashlib.file_digest(contents, "sha256").hexdigest()
    if actual != ASSET_SHA256:
        raise RuntimeError(f"unexpected official release checksum {actual}")
    with tarfile.open(archive, "r:gz") as package:
        matches = [entry for entry in package.getmembers() if entry.isfile() and Path(entry.name).name == "engram"]
        if len(matches) != 1:
            raise RuntimeError("expected one regular Engram binary in the release archive")
        with package.extractfile(matches[0]) as contents, destination.open("xb") as output:
            shutil.copyfileobj(contents, output)
    destination.chmod(0o700)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path.cwd(), help="clean detached source checkout")
    parser.add_argument("--revision", default=MAIN, help="exact upstream commit to build")
    parser.add_argument("--rounds", type=int, default=2, choices=range(1, 6))
    parser.add_argument("--output", type=Path, required=True, help="new results JSON file outside the source checkout")
    args = parser.parse_args()
    if (platform.system(), platform.machine()) != ("Darwin", "arm64"):
        parser.error("run this benchmark on native macOS ARM64")
    if not re.fullmatch("[a-f0-9]{40}", args.revision):
        parser.error("--revision requires the full Git commit ID")
    repo, result_path = args.repo.resolve(), args.output.resolve()
    if result_path.exists() or result_path.is_relative_to(repo) or not result_path.parent.is_dir():
        parser.error("--output must be a new file in an existing directory outside the source checkout")
    if run(["git", "rev-parse", "HEAD"], cwd=repo) != args.revision or run(["git", "status", "--porcelain"], cwd=repo):
        parser.error("expected a clean checkout at the requested revision")
    with tempfile.TemporaryDirectory(prefix="engram-1967-native-macos-") as temp:
        root = Path(temp).resolve()
        archive, official, candidate = root / ASSET, root / "engram-3.4.0", root / "engram-main"
        run(["curl", "--fail", "--silent", "--show-error", "--location", "--max-time", "120",
             "--output", archive, f"{REPOSITORY}/releases/download/{RELEASE}/{ASSET}"], timeout=130)
        extract_release(archive, official)
        build_env = os.environ | {"GOOS": "darwin", "GOARCH": "arm64", "GOFLAGS": "", "GOWORK": "off"}
        run(["go", "build", "-mod=readonly", "-buildvcs=true", "-o", candidate, "./cmd/engram"],
            cwd=repo, env=build_env, timeout=300)
        metadata = run(["go", "version", "-m", candidate])
        if f"vcs.revision={args.revision}" not in metadata or "vcs.modified=false" not in metadata:
            raise RuntimeError("Go build metadata does not match the clean checked-out revision")
        results = {"revision": args.revision, "official_asset": ASSET, "official_sha256": ASSET_SHA256,
                   "os": platform.platform(), "macos_version": platform.mac_ver()[0], "arch": platform.machine(),
                   "go": run(["go", "version"], cwd=repo), "health_period_s": PERIOD,
                   "runs": [measure(root, candidate, "canonical-main", args.rounds),
                            measure(root, official, RELEASE, args.rounds)]}
        with result_path.open("x", encoding="utf-8") as output:
            json.dump(results, output, indent=2)
            output.write("\n")


if __name__ == "__main__":
    main()
