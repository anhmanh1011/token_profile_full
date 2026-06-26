"""Unit tests for license resolution (pure, no network)."""
from creator import resolve_license_sku, A1_STUDENTS_PART_NUMBER

A1_ID = "314c4481-f395-4525-be8b-2ec4bb1e9d91"
E3_ID = "05e9a617-0261-4cee-bb44-138d3ef5d965"


def _sku(sku_id: str, part: str, enabled: int, consumed: int) -> dict:
    return {
        "skuId": sku_id,
        "skuPartNumber": part,
        "prepaidUnits": {"enabled": enabled},
        "consumedUnits": consumed,
    }


def test_auto_prefers_a1_students_when_available():
    skus = [
        _sku(E3_ID, "ENTERPRISEPACK", 10, 0),
        _sku(A1_ID, A1_STUDENTS_PART_NUMBER, 10, 3),
    ]
    assert resolve_license_sku(skus, "auto") == A1_ID


def test_none_behaves_like_auto():
    skus = [_sku(A1_ID, A1_STUDENTS_PART_NUMBER, 5, 0)]
    assert resolve_license_sku(skus, None) == A1_ID


def test_alias_a1_students_resolves_to_part_number():
    skus = [_sku(A1_ID, A1_STUDENTS_PART_NUMBER, 5, 0)]
    assert resolve_license_sku(skus, "a1-students") == A1_ID


def test_auto_falls_back_to_first_available_when_a1_full():
    skus = [
        _sku(A1_ID, A1_STUDENTS_PART_NUMBER, 5, 5),   # no free seat
        _sku(E3_ID, "ENTERPRISEPACK", 10, 2),         # free seat
    ]
    assert resolve_license_sku(skus, "auto") == E3_ID


def test_explicit_guid_used_when_available():
    skus = [
        _sku(A1_ID, A1_STUDENTS_PART_NUMBER, 5, 0),
        _sku(E3_ID, "ENTERPRISEPACK", 10, 0),
    ]
    assert resolve_license_sku(skus, E3_ID) == E3_ID


def test_explicit_guid_no_fallback_when_full():
    skus = [
        _sku(E3_ID, "ENTERPRISEPACK", 10, 10),        # pinned but full
        _sku(A1_ID, A1_STUDENTS_PART_NUMBER, 5, 0),   # free, but must NOT be chosen
    ]
    assert resolve_license_sku(skus, E3_ID) is None


def test_explicit_part_number_no_fallback_when_absent():
    skus = [_sku(A1_ID, A1_STUDENTS_PART_NUMBER, 5, 0)]
    assert resolve_license_sku(skus, "ENTERPRISEPACK") is None


def test_empty_sku_list_returns_none():
    assert resolve_license_sku([], "auto") is None


from unittest.mock import MagicMock
from creator import BulkUserCreator


def _creator(usage_location="US", license_sku=None):
    mgr = MagicMock()
    mgr.domain = "tenant1.example"
    return BulkUserCreator(mgr, count=1, license_sku=license_sku, usage_location=usage_location)


def test_usage_location_used_in_user_data():
    c = _creator(usage_location="VN")
    data = c._generate_user_data()
    assert data["usageLocation"] == "VN"


def test_usage_location_defaults_to_us():
    c = _creator()
    assert c._generate_user_data()["usageLocation"] == "US"


def test_preference_stored_separately_from_resolved_sku():
    c = _creator(license_sku="a1-students")
    assert c.license_pref == "a1-students"
    assert c.license_sku is None  # not resolved until run()
