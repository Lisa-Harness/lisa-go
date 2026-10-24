"""Differential crosscheck probe: drive the REAL Python SqliteMemoryStore
and the lisa-memory CLI server over the same fixture, capture outputs to
JSON for the Go side (connector/memory/crosscheck_test.go) to replay.

Mirrors the thread port's py_probe.py harness. Run from the lisa-go repo:
    C:/Code/lisa/.venv/Scripts/python.exe connector/memory/py_probe.py <out.json>

The fixture is a fresh SQLite DB built here (schema parity guaranteed by
running the store itself), then each probe case executes against the store
and its output is serialized. IDs are pinned by insertion order (the store
uses AUTOINCREMENT, so the first insert is id 1).
"""

from __future__ import annotations

import asyncio
import json
import shutil
import sys
import tempfile
from pathlib import Path

from sdk.memory.store import SqliteMemoryStore


def _write(**kw) -> dict:
    base = {
        "user_slug": "adam",
        "content": "",
        "memory_scope": "personal",
        "workspace_id": "default",
        "surface_id": "",
        "memory_tier": "long_term",
        "content_json": {},
        "operation_id": "test",
    }
    base.update(kw)
    return base


async def main(out_path: str) -> None:
    tmp = Path(tempfile.mkdtemp(prefix="lisa-mem-crosscheck-"))
    db_path = tmp / "memory.db"
    store = SqliteMemoryStore(db_path=db_path)

    outputs: dict[str, str] = {}

    # ── fixture writes (insertion order pins ids 1..6) ────────────────────
    r1 = await store.record_memory(_write(
        content="We postponed the Redis migration.",
        memory_tier="daily",
    ))
    r2 = await store.record_memory(_write(
        content="We chose PostgreSQL for the control plane.",
        salience=0.8,
        content_json={"decider": "architecture-meeting", "status": "final"},
    ))
    r3 = await store.record_memory(_write(
        content="channel-only note about ALPHA_TOKEN",
        surface_id="chan",
    ))
    r4 = await store.record_memory(_write(
        content="nadia private note about postgres", user_slug="nadia",
    ))
    r5 = await store.record_memory(_write(
        content="episodic two", memory_kind="semantic",
    ))
    r6 = await store.record_memory(_write(
        content="retired postgres note on chan", surface_id="chan", memory_tier="daily",
    ))
    await store.retire_memory(memory_id=r6.id, user_slug="adam", surface_id="chan")
    # Snapshot the DB at the pre-recall state: every output below was
    # captured incrementally against the live store, and the Go replay
    # must start from this same base state (access counters all 0).
    pre_recall_db = tmp / "memory.pre-recall.db"
    shutil.copy2(db_path, pre_recall_db)
    outputs["fixture"] = json.dumps({
        "ids": [r1.id, r2.id, r3.id, r4.id, r5.id, r6.id],
        "pre_recall_db_path": str(pre_recall_db),
    })

    # ── record shape ──────────────────────────────────────────────────────
    got = await store.get_memory(memory_id=r2.id, user_slug="adam")
    outputs["get_record"] = json.dumps(got.model_dump(mode="json"), default=str)

    # ── list semantics ────────────────────────────────────────────────────
    outputs["list_default"] = json.dumps([
        r.model_dump(mode="json") for r in
        await store.list_memories(user_slug="adam")
    ], default=str)
    outputs["list_channel"] = json.dumps([
        r.model_dump(mode="json") for r in
        await store.list_memories(user_slug="adam", surface_id="chan")
    ], default=str)
    outputs["list_status_none"] = json.dumps([
        r.model_dump(mode="json") for r in
        await store.list_memories(user_slug="adam", status=None)
    ], default=str)
    outputs["list_kind_semantic"] = json.dumps([
        r.model_dump(mode="json") for r in
        await store.list_memories(user_slug="adam", memory_kind="semantic")
    ], default=str)
    outputs["list_nadia"] = json.dumps([
        r.model_dump(mode="json") for r in
        await store.list_memories(user_slug="nadia")
    ], default=str)
    outputs["count_active"] = str(await store.count_memories(user_slug="adam"))
    outputs["count_status_none"] = str(await store.count_memories(user_slug="adam", status=None))
    outputs["count_channel"] = str(await store.count_memories(user_slug="adam", surface_id="chan"))

    # ── recall ranking / isolation ────────────────────────────────────────
    outputs["recall_postgres"] = await store.recall(
        "chose postgresql control", user_slug="adam", limit=5)
    outputs["recall_records_json"] = json.dumps([
        r.model_dump(mode="json") for r in
        await store.recall_records(
            "postgres", user_slug="adam", limit=5)
    ], default=str)
    outputs["recall_no_match"] = await store.recall(
        "zzz no match", user_slug="adam", limit=2)
    outputs["recall_channel_isolated"] = await store.recall(
        "postgres", user_slug="adam", surface_id="chan", limit=5)
    outputs["recall_nadia"] = await store.recall(
        "postgres", user_slug="nadia", limit=5)

    # ── retire visibility ─────────────────────────────────────────────────
    outputs["recall_excludes_retired_by_scope"] = await store.recall(
        "retired postgres", user_slug="adam", limit=5)

    # ── access tracking ───────────────────────────────────────────────────
    await store.recall("postgres", user_slug="adam", limit=3)
    got2 = await store.get_memory(memory_id=r2.id, user_slug="adam")
    outputs["access_tracking"] = json.dumps({
        "access_count": got2.access_count,
        "last_accessed_at": got2.last_accessed_at,
    }, default=str)

    store.close()

    # The retired row's final shape (read via a fresh store on the same DB).
    store2 = SqliteMemoryStore(db_path=db_path)
    retired = await store2.get_memory(memory_id=r6.id, user_slug="adam", surface_id="chan")
    outputs["retired_record"] = json.dumps(retired.model_dump(mode="json"), default=str)
    store2.close()

    Path(out_path).write_text(json.dumps(outputs, indent=2), encoding="utf-8")
    print(f"wrote {out_path} ({len(outputs)} cases); db at {db_path}")


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1]))