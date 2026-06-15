import unittest
import queue

import app as service_app
from pool_config import PoolConfig


class DeleteUsersValidationTest(unittest.TestCase):
    def setUp(self) -> None:
        service_app.app.config["TESTING"] = True
        service_app._pools.clear()
        self.client = service_app.app.test_client()

    def test_delete_requires_json_email_list(self) -> None:
        response = self.client.post("/users/delete", json={})

        self.assertEqual(response.status_code, 400)
        self.assertIn("emails", response.get_json()["error"])

    def test_delete_rejects_too_many_emails(self) -> None:
        emails = [f"user{i}@example.com" for i in range(service_app.MAX_DELETE_EMAILS + 1)]

        response = self.client.post("/users/delete", json={"emails": emails})

        self.assertEqual(response.status_code, 400)
        self.assertIn("at most", response.get_json()["error"])

    def test_delete_rejects_invalid_email(self) -> None:
        response = self.client.post("/users/delete", json={"emails": ["bad email"]})

        self.assertEqual(response.status_code, 400)
        self.assertIn("invalid email", response.get_json()["error"])

    def test_delete_valid_payload_requires_initialized_service(self) -> None:
        response = self.client.post(
            "/users/delete",
            json={"emails": ["user@example.com"]},
        )

        self.assertEqual(response.status_code, 503)
        self.assertIn("not initialised", response.get_json()["error"])

    def test_delete_rejects_email_outside_pool_scope(self) -> None:
        pool_config = PoolConfig(
            pool_id="bot_p01",
            refresh_token="redacted",
            tenant_id="tenant",
            username="admin",
            domain="example.com",
            proxy=None,
            email_file="email1.txt",
            bot_prefix="bot_p01_",
        )
        service_app._pools["bot_p01"] = service_app.PoolState(
            config=pool_config,
            token_mgr=None,
            token_queue=queue.Queue(),
        )

        response = self.client.post(
            "/pools/bot_p01/users/delete",
            json={"emails": ["bot_p02_abcd@example.com"]},
        )

        self.assertEqual(response.status_code, 400)
        self.assertIn("does not match pool prefix", response.get_json()["error"])


if __name__ == "__main__":
    unittest.main()
