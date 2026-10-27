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
    dim_geography = ibis.memtable(
        {
            "postcode": ["LS1 1AA", "LS1 1AB", "YO1 1AA"],
            "is_live": [True, True, False],
            "introduced_month": [dt.date(1980, 1, 1)] * 3,
            "terminated_month": [None, None, dt.date(2010, 1, 1)],
            "latitude": [53.8, 53.8, 53.96],
            "longitude": [-1.55, -1.55, -1.08],
            "oa_code": ["E00000001", "E00000002", "E00000003"],
            "lsoa_code": ["E01000001", "E01000001", "E01000002"],
            "msoa_code": ["E02000001", "E02000001", "E02000002"],
            "ward_code": ["E05000001", "E05000001", "E05000002"],
            "local_authority_code": ["E08000035", "E08000035", "E06000014"],
            "constituency_code": ["E14000001", "E14000001", "E14000002"],
            "police_force_area_code": ["E23000010", "E23000010", "E23000009"],
            "region_code": ["E12000003"] * 3,
            "country_code": ["E92000001"] * 3,
            "imd_rank": [100, 100, 20000],
        },
        schema={
            "postcode": "string",
            "is_live": "boolean",
            "introduced_month": "date",
            "terminated_month": "date",
            "latitude": "float64",
            "longitude": "float64",
            "oa_code": "string",
            "lsoa_code": "string",
            "msoa_code": "string",
            "ward_code": "string",
            "local_authority_code": "string",
            "constituency_code": "string",
            "police_force_area_code": "string",
            "region_code": "string",
            "country_code": "string",
            "imd_rank": "int32",
        },
    )
    dim_lsoa = ibis.memtable(
        {
            "lsoa_code": ["E01000001", "E01000002"],
            "msoa_code": ["E02000001", "E02000002"],
            "local_authority_code": ["E08000035", "E06000014"],
            "police_force_area_code": ["E23000010", "E23000009"],
            "region_code": ["E12000003"] * 2,
            "country_code": ["E92000001"] * 2,
            "postcodes": [2, 1],
            "live_postcodes": [2, 0],
        },
        schema={
            "lsoa_code": "string",
            "msoa_code": "string",
            "local_authority_code": "string",
            "police_force_area_code": "string",
            "region_code": "string",
            "country_code": "string",
            "postcodes": "int64",
            "live_postcodes": "int64",
        },
    )
    fct_price_paid = ibis.memtable(
        {
            "transaction_id": ["a", "b", "c", "d"],
            "date_of_transfer": [dt.date(2025, 1, 5), dt.date(2025, 1, 9), dt.date(2025, 2, 1), dt.date(2025, 2, 3)],
            "month": [dt.date(2025, 1, 1), dt.date(2025, 1, 1), dt.date(2025, 2, 1), dt.date(2025, 2, 1)],
            "price": [100_000, 300_000, 200_000, 50_000],
            "postcode": ["LS1 1AA", "LS1 1AB", "YO1 1AA", None],
            "property_type": ["F", "D", "T", "O"],
            "is_new_build": [False, False, True, False],
            "duration": ["L", "F", "F", "F"],
            "ppd_category_type": ["A", "A", "A", "B"],
        },
        schema={
            "transaction_id": "string",
            "date_of_transfer": "date",
            "month": "date",
            "price": "int64",
            "postcode": "string",
            "property_type": "string",
            "is_new_build": "boolean",
            "duration": "string",
            "ppd_category_type": "string",
        },
    )
    fct_crime_street = ibis.memtable(
        {
            "crime_id": ["x", None, "y"],
            "month": [dt.date(2025, 1, 1)] * 3,
            "crime_type": ["Burglary", "Anti-social behaviour", "Burglary"],
            "last_outcome_category": ["Under investigation", None, "Under investigation"],
            "reported_by": ["West Yorkshire Police", "West Yorkshire Police", "North Yorkshire Police"],
            "falls_within": ["West Yorkshire Police", "West Yorkshire Police", "North Yorkshire Police"],
            "lsoa_code": ["E01000001", "E01000001", "E01000002"],
            "reported_lsoa_code": ["E01000001", "E01000001", "E01099999"],
            "is_lsoa_approximate": [False, False, True],
            "latitude": [53.8, 53.8, 53.96],
            "longitude": [-1.55, -1.55, -1.08],
        },
        schema={
            "crime_id": "string",
            "month": "date",
            "crime_type": "string",
            "last_outcome_category": "string",
            "reported_by": "string",
            "falls_within": "string",
            "lsoa_code": "string",
            "reported_lsoa_code": "string",
            "is_lsoa_approximate": "boolean",
            "latitude": "float64",
            "longitude": "float64",
        },
    )
    tables = {
        "price_paid_monthly": price_paid_monthly,
        "dim_geography": dim_geography,
        "dim_lsoa": dim_lsoa,
        "fct_price_paid": fct_price_paid,
        "fct_crime_street": fct_crime_street,
    }
    return load_models(tables, HERE / "models.yml")


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


def test_prices_and_crime_by_local_authority(models):
    prices = (
        models["price_paid"]
        .group_by("geography.local_authority_code")
        .aggregate("sales", "median_price")
        .order_by("geography.local_authority_code")
        .execute()
    )
    # The sale without a postcode is in no local authority, not dropped.
    assert prices["geography.local_authority_code"].tolist() == ["E06000014", "E08000035", None]
    assert prices["sales"].tolist() == [1, 2, 1]
    assert prices["median_price"].tolist() == [200_000, 200_000, 50_000]

    crime = (
        models["crime"]
        .group_by("lsoa.local_authority_code")
        .aggregate("crimes")
        .order_by("lsoa.local_authority_code")
        .execute()
    )
    assert crime["lsoa.local_authority_code"].tolist() == ["E06000014", "E08000035"]
    assert crime["crimes"].tolist() == [1, 2]


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
