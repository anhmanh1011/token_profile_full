import json
import tempfile
import unittest
from pathlib import Path

from pool_config import load_pool_configs


class PoolConfigTest(unittest.TestCase):
    def test_loads_pool_id_from_bot_prefix(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            config_path = Path(tmp) / "admin_token_config_global.json"
            config_path.write_text(
                json.dumps(
                    [
                        {
                            "refresh_token": "secret",
                            "tenant_id": "tenant",
                            "username": "admin",
                            "domain": "example.com",
                            "proxy": "127.0.0.1:1080",
                            "email_file": "email1.txt",
                            "bot_prefix": "bot_p01_",
                        }
                    ]
                ),
                encoding="utf-8",
            )

            pools = load_pool_configs(config_path)

        self.assertEqual(len(pools), 1)
        self.assertEqual(pools[0].pool_id, "bot_p01")
        self.assertEqual(pools[0].bot_prefix, "bot_p01_")

    def test_rejects_duplicate_pool_id(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            config_path = Path(tmp) / "admin_token_config_global.json"
            entry = {
                "refresh_token": "secret",
                "tenant_id": "tenant",
                "username": "admin",
                "domain": "example.com",
                "proxy": "",
                "email_file": "email1.txt",
                "bot_prefix": "bot_p01_",
            }
            config_path.write_text(json.dumps([entry, dict(entry)]), encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "duplicate pool_id"):
                load_pool_configs(config_path)


if __name__ == "__main__":
    unittest.main()
