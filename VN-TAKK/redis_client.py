import json
import time
import redis
import logging

logger = logging.getLogger(__name__)

class TokenRedis:
    def __init__(self, host='127.0.0.1', port=6379, db=0):
        self.r = redis.Redis(host=host, port=port, db=db, decode_responses=True)
        self.r.ping()
        logger.info(f"Redis connected: {host}:{port}")

    def push_token(self, email, password, refresh_token, tenant_id):
        """Push a fresh token to Redis"""
        token_data = json.dumps({
            "email": email,
            "password": password,
            "refresh_token": refresh_token,
            "tenant_id": tenant_id,
            "created_at": int(time.time()),
            "exhausted_at": 0
        })
        key = f"tokens:{tenant_id}:fresh"
        self.r.rpush(key, token_data)

    def get_accounts_to_login(self, tenant_id, all_accounts):
        """Determine which accounts need to be logged in."""
        accounts_in_redis = set()
        cooldown_ready = []

        used_key = f"tokens:{tenant_id}:used"
        used_tokens = self.r.lrange(used_key, 0, -1)
        now = int(time.time())

        for val in used_tokens:
            token = json.loads(val)
            accounts_in_redis.add(token["email"])
            if token.get("exhausted_at", 0) > 0 and (now - token["exhausted_at"]) >= 86400:
                self.r.lrem(used_key, 1, val)
                cooldown_ready.append(f"{token['email']}:{token['password']}")

        for suffix in ["fresh", "in_use"]:
            key = f"tokens:{tenant_id}:{suffix}"
            items = self.r.lrange(key, 0, -1)
            for val in items:
                token = json.loads(val)
                accounts_in_redis.add(token["email"])

        new_accounts = []
        for account in all_accounts:
            email = account.split(":")[0]
            if email not in accounts_in_redis:
                new_accounts.append(account)

        result = cooldown_ready + new_accounts
        logger.info(
            f"[{tenant_id}] Accounts to login: {len(result)} "
            f"(cooldown_ready={len(cooldown_ready)}, new={len(new_accounts)}, "
            f"in_redis={len(accounts_in_redis)})"
        )
        return result

    def stats(self, tenant_id):
        """Return queue lengths"""
        return {
            "fresh": self.r.llen(f"tokens:{tenant_id}:fresh"),
            "in_use": self.r.llen(f"tokens:{tenant_id}:in_use"),
            "used": self.r.llen(f"tokens:{tenant_id}:used"),
        }
