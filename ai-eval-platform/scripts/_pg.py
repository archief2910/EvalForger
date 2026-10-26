"""PostgreSQL helpers shared by the harness scripts.

The control plane used to be a SQLite file inside DATA_DIR, so a throw-away
DATA_DIR isolated a run. PostgreSQL lives outside that directory, so isolation
now comes from a schema per run: the harness picks a name, the server creates
and pins it with CONTROL_DB_SCHEMA, and the harness drops it on the way out.

Direct inspection (the fault harness checks job rows while the server is down)
goes through psql, because the harness deliberately has no database driver
dependency beyond the standard library.
"""

import os
import secrets
import shutil
import subprocess
from urllib.parse import unquote, urlparse

DEFAULT_URL = "postgres://evalforge@127.0.0.1:55432/evalforge?sslmode=disable"


def control_url():
    return os.environ.get("TEST_CONTROL_DB_URL") or os.environ.get("CONTROL_DB_URL") or DEFAULT_URL


def schema(prefix="harness"):
    """A fresh, valid schema name for one harness run."""
    return f"{prefix}_{os.getpid()}_{secrets.token_hex(3)}"


def _psql():
    exe = shutil.which("psql")
    if exe:
        return exe
    for candidate in (
        r"C:\Program Files\PostgreSQL\16\bin\psql.exe",
        r"C:\Program Files\PostgreSQL\15\bin\psql.exe",
        "/usr/lib/postgresql/16/bin/psql",
        "/opt/homebrew/bin/psql",
    ):
        if os.path.exists(candidate):
            return candidate
    raise SystemExit("psql not found: install the PostgreSQL client to run this harness")


def run_sql(sql, schema_name=None, timeout=20):
    """Run SQL and return rows as tuples of strings (psql's unaligned output)."""
    url = urlparse(control_url())
    args = [
        _psql(), "-X", "-q", "-v", "ON_ERROR_STOP=1",
        "-h", url.hostname or "127.0.0.1",
        "-p", str(url.port or 5432),
        "-U", unquote(url.username or "postgres"),
        "-d", (url.path or "/postgres").lstrip("/"),
        "-t", "-A", "-F", "|",
    ]
    if schema_name:
        args += ["-c", f'SET search_path TO "{schema_name}"']
    args += ["-c", sql]
    env = dict(os.environ)
    if url.password:
        env["PGPASSWORD"] = unquote(url.password)
    out = subprocess.run(args, capture_output=True, text=True, timeout=timeout, env=env)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.strip() or f"psql exited {out.returncode}")
    rows = []
    for line in out.stdout.splitlines():
        if not line.strip():
            continue
        rows.append(tuple(line.split("|")))
    return rows


def drop_schema(name):
    """Best-effort cleanup; a leftover schema must never fail a passing run."""
    if not name:
        return
    try:
        run_sql(f'DROP SCHEMA IF EXISTS "{name}" CASCADE')
    except Exception:
        pass


def literal(value):
    """Quote a Python value for the simple ?-substitution used by the harness."""
    if value is None:
        return "NULL"
    if isinstance(value, bool):
        return "TRUE" if value else "FALSE"
    if isinstance(value, (int, float)):
        return str(value)
    return "'" + str(value).replace("'", "''") + "'"


def bind(sql, args):
    for value in args:
        sql = sql.replace("?", literal(value), 1)
    return sql
