import unittest

import app as service_app


class DeleteUsersValidationTest(unittest.TestCase):
    def setUp(self) -> None:
        service_app.app.config["TESTING"] = True
        service_app._token_mgr = None
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


if __name__ == "__main__":
    unittest.main()
