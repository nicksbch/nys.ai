"""semantic: the Boring Semantic Layer over the lake's marts, served as an MCP
server over HTTP (streamable HTTP at /mcp).

    python server.py serve   # the MCP server
    python server.py check   # load the models against the lake, then exit

The lake is attached read-only from the DuckLake catalog. The catalog's
PostgreSQL credentials come from libpq's PG* env vars, the S3 ones from the
AWS_* vars, as for lake-ui.
"""

import argparse
import asyncio
import hmac
import os
import socket
import sys
from pathlib import Path
from urllib.parse import urlparse

import ibis
import uvicorn
from boring_semantic_layer import MCPSemanticModel, from_yaml
from fastmcp.server.middleware import Middleware
from starlette.responses import JSONResponse, PlainTextResponse

HERE = Path(__file__).resolve().parent
LAKE = "lake"


def ql(s: str) -> str:
    """Quotes s as an SQL string literal."""
    return "'" + s.replace("'", "''") + "'"


def connect(catalog: str, endpoint: str, state_dir: str | None):
    """Returns an in-memory DuckDB with the lake attached read-only as "lake"."""
    con = ibis.duckdb.connect()
    if state_dir:
        con.raw_sql(f"SET extension_directory = {ql(os.path.join(state_dir, 'extensions'))}")
        con.raw_sql(f"SET temp_directory = {ql(os.path.join(state_dir, 'tmp'))}")
    for ext in ("httpfs", "ducklake", "postgres"):
        con.raw_sql(f"INSTALL {ext}; LOAD {ext}")

    region = os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION") or "garage"
    secret = ["TYPE s3", f"REGION {ql(region)}", "URL_STYLE 'path'"]
    if key_id := os.environ.get("AWS_ACCESS_KEY_ID"):
        secret += [f"KEY_ID {ql(key_id)}", f"SECRET {ql(os.environ.get('AWS_SECRET_ACCESS_KEY', ''))}"]
    if endpoint:
        u = urlparse(endpoint)
        if not u.netloc:
            raise ValueError(f"endpoint {endpoint!r}: want a URL like http://host:3900")
        secret += [f"ENDPOINT {ql(u.netloc)}", f"USE_SSL {str(u.scheme == 'https').lower()}"]
    con.raw_sql(f"CREATE TEMPORARY SECRET semantic_s3 ({', '.join(secret)})")
    con.raw_sql(f"ATTACH {ql(catalog)} AS {LAKE} (READ_ONLY)")
    return con


def load_models(tables: dict, path: Path) -> dict:
    """Loads the semantic models in path over tables (name -> ibis table)."""
    return from_yaml(str(path), tables=tables)


def mart_tables(con, schema: str) -> dict:
    """Every table in the lake's marts schema, by name."""
    db = (LAKE, schema)
    return {name: con.table(name, database=db) for name in con.list_tables(database=db)}


class OneAtATime(Middleware):
    """Runs one tool call at a time: the tools run in threads, and they share
    one DuckDB connection, which is not safe to use from several at once."""

    def __init__(self):
        self.lock = asyncio.Lock()

    async def on_call_tool(self, context, call_next):
        async with self.lock:
            return await call_next(context)


class BearerAuth:
    """ASGI middleware: every path but /healthz needs "Authorization: Bearer <token>"."""

    def __init__(self, app, token: str):
        self.app = app
        self.want = f"Bearer {token}".encode()

    async def __call__(self, scope, receive, send):
        if scope["type"] == "http" and scope["path"] != "/healthz":
            got = dict(scope["headers"]).get(b"authorization", b"")
            if not hmac.compare_digest(got, self.want):
                await JSONResponse({"error": "unauthorized"}, status_code=401)(scope, receive, send)
                return
        await self.app(scope, receive, send)


def split_listen(listen: str) -> tuple[str, int]:
    host, _, port = listen.rpartition(":")
    return host.strip("[]") or "127.0.0.1", int(port)


def listen_socket(listen: str) -> socket.socket:
    """Binds listen. "[::]" takes IPv4 too, as it does for the Go services:
    asyncio would bind it IPv6 only, and 127.0.0.1 (the hub, the health
    check) could not connect."""
    host, port = split_listen(listen)
    family = socket.AF_INET6 if ":" in host else socket.AF_INET
    sock = socket.socket(family, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if family == socket.AF_INET6:
        sock.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
    sock.bind((host, port))
    return sock


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("command", choices=["serve", "check"])
    p.add_argument("-listen", default="127.0.0.1:8097", help="address to serve on")
    p.add_argument("-models", default=str(HERE / "models.yml"), help="semantic models (YAML)")
    p.add_argument("-schema", default="marts", help="lake schema the models' tables are in")
    p.add_argument("-catalog", default=os.environ.get("SEMANTIC_CATALOG", "ducklake:postgres:dbname=ducklake"),
                   help="DuckLake catalog to attach ($SEMANTIC_CATALOG)")
    p.add_argument("-endpoint-url", default=os.environ.get("AWS_ENDPOINT_URL", "http://192.168.1.169:3900"),
                   help="S3 endpoint ($AWS_ENDPOINT_URL)")
    p.add_argument("-state-dir", default=None, help="where DuckDB keeps extensions and temp files")
    p.add_argument("-token", default=os.environ.get("SEMANTIC_MCP_TOKEN", ""),
                   help="bearer token MCP clients must send ($SEMANTIC_MCP_TOKEN); empty = none")
    args = p.parse_args()

    # check exits 1 when the lake can't be reached, 2 when the models don't
    # load against it, like the other services' check.
    try:
        con = connect(args.catalog, args.endpoint_url, args.state_dir)
        tables = mart_tables(con, args.schema)
    except Exception as e:
        print(f"semantic: attaching the lake: {e}", file=sys.stderr)
        return 1
    try:
        models = load_models(tables, Path(args.models))
        for name, model in models.items():
            model.group_by().aggregate(*model.get_measures()).limit(1).execute()
    except Exception as e:
        print(f"semantic: {args.models}: {e}", file=sys.stderr)
        return 2
    if args.command == "check":
        print(f"ok: {len(models)} models ({', '.join(models)})")
        return 0

    mcp = MCPSemanticModel(models=models, name="semantic")
    mcp.add_middleware(OneAtATime())

    @mcp.custom_route("/healthz", methods=["GET"])
    async def healthz(_request):
        return PlainTextResponse("ok")

    middleware = []
    if args.token:
        from starlette.middleware import Middleware as ASGIMiddleware

        middleware.append(ASGIMiddleware(BearerAuth, token=args.token))
    app = mcp.http_app(path="/mcp", middleware=middleware)
    uvicorn.Server(uvicorn.Config(app, log_level="info")).run(sockets=[listen_socket(args.listen)])
    return 0


if __name__ == "__main__":
    sys.exit(main())
