"""Bot-token tenant_id must default to the admin's tenant_id (not scraped-empty).

The tenant_id used for token exchange comes from the admin directory (bot users
live in it). Relying only on the fragile reporting-endpoints header scrape left
some tenants with an empty tenant_id, breaking Get_Profile's token exchange.
"""
from token_getter import BulkTokenGetter, TeamOutLook


def test_bulk_getter_stores_admin_tenant_id():
    g = BulkTokenGetter([], admin_tenant_id="tid-123")
    assert g.admin_tenant_id == "tid-123"


def test_teamoutlook_defaults_tenant_id_to_admin():
    # Constructing TeamOutLook builds a curl_cffi session (offline) — no network.
    obj = TeamOutLook("bot@x.example", "pw", admin_tenant_id="tid-456")
    assert obj.tenant_id == "tid-456"


def test_teamoutlook_tenant_id_empty_without_admin():
    obj = TeamOutLook("bot@x.example", "pw")
    assert obj.tenant_id == ""
