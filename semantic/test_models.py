"""The models in models.yml load and answer queries, over in-memory tables with
the marts' columns (no lake needed)."""

import asyncio
import datetime as dt
import socket

import ibis
import pytest

from server import HERE, BearerAuth, listen_socket, load_models, split_listen


@pytest.fixture
def models():
    price_paid_monthly = ibis.memtable(
        {
            "month": [dt.date(2025, 1, 1), dt.date(2025, 1, 1), dt.date(2025, 2, 1)],
            "district": ["LEEDS", "LEEDS", "YORK"],
            "property_type": ["D", "F", "T"],
            "sales": [10, 30, 5],
            "median_price": [300_000, 150_000, 250_000],
            "avg_price": [320_000, 160_000, 260_000],
        },
        schema={
            "month": "date",
            "district": "string",
            "property_type": "string",
            "sales": "int64",
            "median_price": "int64",
            "avg_price": "int64",
        },
    )
    return load_models({"price_paid_monthly": price_paid_monthly}, HERE / "models.yml")


def test_sales_and_average_by_district(models):
    df = (
        models["price_paid_monthly"]
        .group_by("district")
        .aggregate("sales", "avg_price")
        .order_by("district")
        .execute()
    )
    assert df["district"].tolist() == ["LEEDS", "YORK"]
    assert df["sales"].tolist() == [40, 5]
    # Weighted by sales, not the mean of the monthly averages.
    assert df["avg_price"].tolist() == [200_000, 260_000]


def test_every_measure_aggregates(models):
    for model in models.values():
        model.group_by().aggregate(*model.get_measures()).execute()


def test_split_listen():
    assert split_listen("[::]:8097") == ("::", 8097)
    assert split_listen("127.0.0.1:8097") == ("127.0.0.1", 8097)


def test_listen_any_takes_ipv4():
    with listen_socket("[::]:0") as sock:
        sock.listen()
        port = sock.getsockname()[1]
        with socket.create_connection(("127.0.0.1", port), timeout=3):
            pass


def test_bearer_auth():
    async def app(scope, receive, send):
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b""})

    async def status(path, auth=None):
        sent = []

        async def send(msg):
            sent.append(msg)

        headers = [(b"authorization", auth.encode())] if auth else []
        await BearerAuth(app, "s3cret")({"type": "http", "path": path, "headers": headers}, None, send)
        return sent[0]["status"]

    assert asyncio.run(status("/mcp")) == 401
    assert asyncio.run(status("/mcp", "Bearer wrong")) == 401
    assert asyncio.run(status("/mcp", "Bearer s3cret")) == 200
    assert asyncio.run(status("/healthz")) == 200
