"""CLI argument parsing for app.py."""
import app


def test_defaults():
    ns = app.build_arg_parser().parse_args([])
    assert ns.license_sku == "a1-students"
    assert ns.usage_location == "US"
    assert ns.port == 5000


def test_overrides():
    ns = app.build_arg_parser().parse_args(
        ["--license-sku", "ENTERPRISEPACK", "--usage-location", "VN", "--port", "5001"]
    )
    assert ns.license_sku == "ENTERPRISEPACK"
    assert ns.usage_location == "VN"
    assert ns.port == 5001
