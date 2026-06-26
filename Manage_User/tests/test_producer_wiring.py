"""TokenProducer must forward license + usage_location to BulkUserCreator."""
import queue
from unittest.mock import MagicMock, patch

import producer


def test_produce_batch_forwards_license_and_usage_location():
    mgr = MagicMock()
    mgr.local_ip = None
    p = producer.TokenProducer(
        mgr, queue.Queue(), license_sku="a1-students", usage_location="VN"
    )

    fake_creator = MagicMock()
    fake_creator.run.return_value = {"created_users": [], "failed": 0}

    with patch.object(producer, "BulkUserCreator", return_value=fake_creator) as ctor:
        p._produce_batch(5)

    ctor.assert_called_once_with(
        mgr, 5, license_sku="a1-students", usage_location="VN"
    )
