# Merge Modules — Python API Service + Go App, No Redis

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Redis broker with a Python HTTP API service on localhost; Go app calls API directly for tokens and user deletion. Streaming pipeline reduces startup wait from ~40 min to ~25s.

**Architecture:** Python (Flask) runs as long-running API service with background producer thread that maintains ≥500 tokens in an in-memory queue. Go app is a batch job that fetches tokens on-demand via `GET /tokens/next` and batches user deletions via `POST /users/delete`. Users are named with `bot_` prefix for easy cleanup on restart.

**Tech Stack:** Python (Flask, requests, curl_cffi, threading), Go (net/http, golang.org/x/time/rate)

**Spec:** `docs/superpowers/specs/2026-04-14-merge-modules-no-redis-design.md`

---

## File Structure

### Python — New/Modified Files

| File | Action | Responsibility |
|------|--------|----------------|
| `Manage_User/app.py` | **Create** | Flask app: endpoints, token queue, producer thread |
| `Manage_User/producer.py` | **Create** | Background producer: create users + get tokens, refill queue |
| `Manage_User/cleanup.py` | **Create** | Startup cleanup: list & delete `bot_` prefix users |
| `Manage_User/creator.py` | **Modify** | Change `_generate_user_data()` to use `bot_` prefix |
| `Manage_User/deleter.py` | **Modify** | Add `delete_by_emails()` method (no Redis dependency) |
| `Manage_User/requirements.txt` | **Modify** | Add `flask` |

### Go — New/Modified/Deleted Files

| File | Action | Responsibility |
|------|--------|----------------|
| `Get_Profile/token/api.go` | **Create** | HTTP client for Python API (`/tokens/next`, `/users/delete`) |
| `Get_Profile/token/redis.go` | **Delete** | No longer needed |
| `Get_Profile/main.go` | **Modify** | Replace Redis with API client |
| `Get_Profile/config/config.go` | **Modify** | Replace Redis fields with `APIAddr` |
| `Get_Profile/go.mod` | **Modify** | Remove `go-redis` dependency |

---

## Task 1: Python — `bot_` prefix user naming

**Files:**
- Modify: `Manage_User/creator.py:87-105`

- [ ] **Step 1: Change `_generate_user_data()` to use `bot_` prefix**

Replace the name generation in `creator.py` (lines 87-105):

```python
def _generate_user_data(self) -> dict:
    # bot_ prefix for easy identification and cleanup
    random_id = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
    username = f"bot_{random_id}"
    password = self._generate_password()

    return {
        "userPrincipalName": f"{username}@{self.domain}",
        "displayName": f"Bot {random_id}",
        "mailNickname": username,
        "accountEnabled": True,
        "usageLocation": "US",
        "_password": password,
        "passwordProfile": {
            "password": password,
            "forceChangePasswordNextSignIn": False,
        },
    }
```

- [ ] **Step 2: Verify syntax**

Run: `cd Manage_User && py -m py_compile creator.py`
Expected: No output (success)

- [ ] **Step 3: Commit**

```bash
git add Manage_User/creator.py
git commit -m "refactor(creator): use bot_ prefix for user naming"
```

---

## Task 2: Python — Startup cleanup module

**Files:**
- Create: `Manage_User/cleanup.py`

- [ ] **Step 1: Create `cleanup.py` — list and delete `bot_` users**

```python
"""
Startup Cleanup — Delete all bot_ prefix users from tenant.

Uses Graph API $filter to find app-created users, then batch deletes them.
Called once at Python API service startup for clean slate.
"""
import logging
import queue
import threading
import time
from typing import Optional

import requests
from requests.adapters import HTTPAdapter

logger = logging.getLogger(__name__)

GRAPH_URL = "https://graph.microsoft.com/v1.0"
CLIENT_ID = "1950a258-227b-4e31-a9cf-717495945fc2"
BATCH_SIZE = 20
BOT_PREFIX = "bot_"


class StartupCleaner:
    """Delete all bot_ prefix users from a Microsoft 365 tenant."""

    def __init__(self, admin: dict):
        self.tenant_id = admin["tenant_id"]
        self.refresh_token = admin["refresh_token"]
        self.domain = admin["domain"]

        self.access_token: Optional[str] = None
        self.token_expires: float = 0
        self.token_lock = threading.Lock()

        self.session = requests.Session()
        adapter = HTTPAdapter(pool_connections=50, pool_maxsize=50, max_retries=3)
        self.session.mount("https://", adapter)

    def _get_token(self) -> Optional[str]:
        with self.token_lock:
            if self.access_token and time.time() < self.token_expires:
                return self.access_token

            data = {
                "client_id": CLIENT_ID,
                "scope": "https://graph.microsoft.com/.default offline_access",
                "refresh_token": self.refresh_token,
                "grant_type": "refresh_token",
            }
            token_url = (
                f"https://login.microsoftonline.com/{self.tenant_id}/oauth2/v2.0/token"
            )
            try:
                resp = self.session.post(token_url, data=data, timeout=30)
                if resp.status_code == 200:
                    token_data = resp.json()
                    self.access_token = token_data["access_token"]
                    self.token_expires = (
                        time.time() + token_data.get("expires_in", 3600) - 300
                    )
                    if "refresh_token" in token_data:
                        self.refresh_token = token_data["refresh_token"]
                    return self.access_token
                else:
                    logger.error("Token error: %s", resp.text[:200])
            except requests.RequestException as e:
                logger.error("Token request failed: %s", e)
            return None

    def _list_bot_users(self) -> list[str]:
        """List all userPrincipalName starting with bot_ via Graph API."""
        bot_emails: list[str] = []
        token = self._get_token()
        if not token:
            logger.error("Cannot get token for cleanup")
            return bot_emails

        headers = {"Authorization": f"Bearer {token}"}
        # Graph API $filter: startsWith on userPrincipalName
        url = (
            f"{GRAPH_URL}/users"
            f"?$filter=startsWith(userPrincipalName,'{BOT_PREFIX}')"
            f"&$select=userPrincipalName&$top=999"
        )

        page = 0
        while url:
            try:
                resp = self.session.get(url, headers=headers, timeout=60)
                if resp.status_code == 429:
                    retry_after = int(resp.headers.get("Retry-After", 5))
                    logger.warning("Rate limited, waiting %ds...", retry_after)
                    time.sleep(retry_after)
                    continue
                if resp.status_code != 200:
                    logger.error("List users failed: %d - %s", resp.status_code, resp.text[:200])
                    break

                data = resp.json()
                for user in data.get("value", []):
                    email = user.get("userPrincipalName", "")
                    if email:
                        bot_emails.append(email)

                page += 1
                logger.info("Cleanup list page %d: %d users (total: %d)", page, len(data.get("value", [])), len(bot_emails))
                url = data.get("@odata.nextLink")
            except requests.RequestException as e:
                logger.error("Error listing users: %s", e)
                break

        return bot_emails

    def _batch_delete(self, emails: list[str]) -> dict:
        """Delete users via Graph Batch API. Returns {deleted, failed}."""
        deleted = 0
        failed = 0

        batches = [emails[i:i + BATCH_SIZE] for i in range(0, len(emails), BATCH_SIZE)]
        logger.info("Cleanup: %d users in %d batches", len(emails), len(batches))

        for batch in batches:
            token = self._get_token()
            if not token:
                failed += len(batch)
                continue

            headers = {
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            }
            requests_list = [
                {"id": str(i), "method": "DELETE", "url": f"/users/{email}"}
                for i, email in enumerate(batch)
            ]

            try:
                resp = self.session.post(
                    f"{GRAPH_URL}/$batch",
                    headers=headers,
                    json={"requests": requests_list},
                    timeout=60,
                )
                if resp.status_code == 429:
                    retry_after = int(resp.headers.get("Retry-After", 5))
                    time.sleep(min(retry_after, 30))
                    failed += len(batch)
                    continue

                if resp.status_code == 200:
                    for r in resp.json().get("responses", []):
                        if r["status"] == 204:
                            deleted += 1
                        else:
                            failed += 1
                else:
                    failed += len(batch)

            except requests.RequestException:
                failed += len(batch)

            time.sleep(1)  # Gentle on Graph API

        return {"deleted": deleted, "failed": failed}

    def run(self) -> dict:
        """List all bot_ users and delete them. Returns {found, deleted, failed}."""
        logger.info("=== Startup Cleanup: deleting bot_ users ===")
        bot_emails = self._list_bot_users()

        if not bot_emails:
            logger.info("No bot_ users found, clean slate.")
            return {"found": 0, "deleted": 0, "failed": 0}

        logger.info("Found %d bot_ users to clean up", len(bot_emails))
        result = self._batch_delete(bot_emails)
        logger.info(
            "Cleanup done: %d found, %d deleted, %d failed",
            len(bot_emails), result["deleted"], result["failed"],
        )
        return {"found": len(bot_emails), **result}
```

- [ ] **Step 2: Verify syntax**

Run: `cd Manage_User && py -m py_compile cleanup.py`
Expected: No output (success)

- [ ] **Step 3: Commit**

```bash
git add Manage_User/cleanup.py
git commit -m "feat(cleanup): startup bot_ user cleanup module"
```

---

## Task 3: Python — Background producer module

**Files:**
- Create: `Manage_User/producer.py`

- [ ] **Step 1: Create `producer.py` — background token producer**

```python
"""
Background Producer — Continuously create users and get tokens.

Runs in a background thread, maintains ≥500 tokens in queue.
Creates batch of 100 users at a time, gets their refresh tokens,
and pushes to a thread-safe queue.
"""
import logging
import queue
import threading
import time

from creator import BulkUserCreator
from token_getter import BulkTokenGetter

logger = logging.getLogger(__name__)

MIN_QUEUE_SIZE = 500
BATCH_SIZE = 100
POLL_INTERVAL = 30  # seconds to sleep when queue is full


class TokenProducer:
    """Background producer that keeps token queue filled."""

    def __init__(self, admin: dict, token_queue: queue.Queue):
        self.admin = admin
        self.token_queue = token_queue
        self.running = False
        self.thread: threading.Thread | None = None

        # Stats
        self.total_created = 0
        self.total_tokens = 0
        self.total_failed_create = 0
        self.total_failed_token = 0
        self.stats_lock = threading.Lock()

    def start(self) -> None:
        """Start producer in background thread."""
        self.running = True
        self.thread = threading.Thread(target=self._run, daemon=True, name="producer")
        self.thread.start()
        logger.info("Producer started (min queue: %d, batch: %d)", MIN_QUEUE_SIZE, BATCH_SIZE)

    def stop(self) -> None:
        """Signal producer to stop."""
        self.running = False
        if self.thread:
            self.thread.join(timeout=10)
        logger.info("Producer stopped")

    def stats(self) -> dict:
        with self.stats_lock:
            return {
                "total_created": self.total_created,
                "total_tokens": self.total_tokens,
                "total_failed_create": self.total_failed_create,
                "total_failed_token": self.total_failed_token,
            }

    def _run(self) -> None:
        """Main producer loop."""
        while self.running:
            try:
                current_size = self.token_queue.qsize()
                if current_size >= MIN_QUEUE_SIZE:
                    logger.debug("Queue has %d tokens (>= %d), sleeping %ds",
                                 current_size, MIN_QUEUE_SIZE, POLL_INTERVAL)
                    time.sleep(POLL_INTERVAL)
                    continue

                need = min(MIN_QUEUE_SIZE - current_size, BATCH_SIZE)
                logger.info("Queue has %d tokens, producing %d more", current_size, need)
                self._produce_batch(need)

            except Exception as e:
                logger.error("Producer error: %s", e)
                time.sleep(5)

    def _produce_batch(self, count: int) -> None:
        """Create users, get tokens, push to queue."""
        # Step 1: Create users
        creator = BulkUserCreator(self.admin, count)
        create_result = creator.run()
        created_users = create_result["created_users"]

        with self.stats_lock:
            self.total_created += len(created_users)
            self.total_failed_create += create_result["failed"]

        # Track refreshed admin token
        new_rt = create_result.get("new_refresh_token")
        if new_rt:
            self.admin["refresh_token"] = new_rt

        if not created_users:
            logger.error("No users created in batch")
            return

        logger.info("Created %d users, getting tokens...", len(created_users))

        # Step 2: Get refresh tokens
        getter = BulkTokenGetter(created_users)
        token_result = getter.run()
        tokens = token_result["tokens"]

        with self.stats_lock:
            self.total_tokens += len(tokens)
            self.total_failed_token += token_result["failed"]

        # Step 3: Push tokens to queue
        pushed = 0
        for tok in tokens:
            try:
                self.token_queue.put(tok, timeout=5)
                pushed += 1
            except queue.Full:
                logger.warning("Token queue full, discarding remaining tokens")
                break

        logger.info("Produced %d tokens (created: %d, token_ok: %d, pushed: %d)",
                     count, len(created_users), len(tokens), pushed)
```

- [ ] **Step 2: Verify syntax**

Run: `cd Manage_User && py -m py_compile producer.py`
Expected: No output (success)

- [ ] **Step 3: Commit**

```bash
git add Manage_User/producer.py
git commit -m "feat(producer): background token producer with auto-refill"
```

---

## Task 4: Python — Flask API service

**Files:**
- Create: `Manage_User/app.py`
- Modify: `Manage_User/requirements.txt`

- [ ] **Step 1: Add flask to requirements.txt**

Add `flask>=3.0.0` to `Manage_User/requirements.txt`:

```
redis>=5.0.0
curl_cffi>=0.7.0
requests>=2.31.0
flask>=3.0.0
```

- [ ] **Step 2: Create `app.py` — Flask API service**

```python
"""
Manage_User API Service

Long-running Flask service that:
1. Cleans up bot_ users on startup
2. Runs background producer to maintain ≥500 tokens
3. Serves tokens via GET /tokens/next
4. Accepts batch delete via POST /users/delete
"""
import json
import logging
import queue
import sys
import threading
import time
from pathlib import Path

from flask import Flask, jsonify, request

from cleanup import StartupCleaner
from creator import BulkUserCreator
from deleter import FastBulkDeleter
from producer import TokenProducer

CONFIG_FILE = Path(__file__).parent / "admin_token.json"
LOG_FILE = Path(__file__).parent / "service.log"
TOKEN_QUEUE_MAX = 1000

# Logging
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(LOG_FILE, encoding="utf-8"),
    ],
)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Global state
token_queue: queue.Queue = queue.Queue(maxsize=TOKEN_QUEUE_MAX)
producer: TokenProducer | None = None
admin_config: dict = {}
delete_stats = {"total_deleted": 0, "total_failed": 0}
delete_lock = threading.Lock()


def load_admin_config() -> dict:
    """Load first admin from admin_token.json."""
    with open(CONFIG_FILE, "r", encoding="utf-8") as f:
        admins = json.load(f)
    if not admins:
        raise ValueError("No admins in config")
    return admins[0]  # 1 VPS = 1 admin


@app.route("/tokens/next", methods=["GET"])
def get_next_token():
    """Get next available token from queue."""
    try:
        tok = token_queue.get_nowait()
        return jsonify({
            "email": tok["email"],
            "refresh_token": tok["refresh_token"],
            "tenant_id": tok["tenant_id"],
        }), 200
    except queue.Empty:
        return jsonify({"waiting": True, "queue_size": 0}), 202


@app.route("/users/delete", methods=["POST"])
def delete_users():
    """Batch delete users by email."""
    data = request.get_json()
    if not data or "emails" not in data:
        return jsonify({"error": "missing 'emails' field"}), 400

    emails = data["emails"]
    if not emails:
        return jsonify({"deleted": 0, "failed": 0}), 200

    # Use deleter's batch delete via Graph API
    deleter = FastBulkDeleter(admin_config, auto_confirm=True)
    results = deleter._process_batch(emails)

    deleted = sum(1 for r in results if r["success"])
    failed = sum(1 for r in results if not r["success"])

    with delete_lock:
        delete_stats["total_deleted"] += deleted
        delete_stats["total_failed"] += failed

    return jsonify({"deleted": deleted, "failed": failed}), 200


@app.route("/status", methods=["GET"])
def get_status():
    """Get service status."""
    producer_stats = producer.stats() if producer else {}
    with delete_lock:
        d_stats = delete_stats.copy()

    return jsonify({
        "queue_size": token_queue.qsize(),
        "total_created": producer_stats.get("total_created", 0),
        "total_tokens": producer_stats.get("total_tokens", 0),
        "total_deleted": d_stats["total_deleted"],
        "total_failed_delete": d_stats["total_failed"],
    }), 200


def start_service(host: str = "0.0.0.0", port: int = 5000) -> None:
    """Initialize and start the API service."""
    global admin_config, producer

    logger.info("=" * 60)
    logger.info("  Manage_User API Service Starting")
    logger.info("=" * 60)

    # Load config
    admin_config = load_admin_config()
    logger.info("Loaded admin: %s", admin_config.get("domain", "?"))

    # Startup cleanup
    cleaner = StartupCleaner(admin_config)
    cleanup_result = cleaner.run()
    logger.info("Cleanup: %s", cleanup_result)

    # Start producer
    producer = TokenProducer(admin_config, token_queue)
    producer.start()

    logger.info("API service ready on %s:%d", host, port)
    app.run(host=host, port=port, threaded=True)


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Manage_User API Service")
    parser.add_argument("--host", default="0.0.0.0", help="Bind host")
    parser.add_argument("--port", type=int, default=5000, help="Bind port")
    args = parser.parse_args()

    start_service(host=args.host, port=args.port)
```

- [ ] **Step 3: Install flask**

Run: `cd Manage_User && pip install flask`

- [ ] **Step 4: Verify syntax of all Python files**

Run: `cd Manage_User && py -m py_compile app.py && py -m py_compile producer.py && py -m py_compile cleanup.py && py -m py_compile creator.py`
Expected: No output (all pass)

- [ ] **Step 5: Commit**

```bash
git add Manage_User/app.py Manage_User/requirements.txt
git commit -m "feat(api): Flask API service with /tokens/next, /users/delete, /status"
```

---

## Task 5: Python — Modify deleter for direct email deletion

**Files:**
- Modify: `Manage_User/deleter.py`

The `/users/delete` endpoint needs to call `_process_batch()` directly with a list of emails. Currently `FastBulkDeleter.__init__` requires `queue_suffix` and creates a Redis client. We need to make it work without Redis for the API use case.

- [ ] **Step 1: Make Redis optional in FastBulkDeleter**

In `deleter.py`, modify `__init__` to make `queue_suffix` optional and skip Redis when not provided:

Change lines 32-68. Replace:

```python
    def __init__(
        self,
        admin: dict,
        queue_suffix: str,
        workers: int = DEFAULT_WORKERS,
        auto_confirm: bool = False,
    ):
```

With:

```python
    def __init__(
        self,
        admin: dict,
        queue_suffix: str = "",
        workers: int = DEFAULT_WORKERS,
        auto_confirm: bool = False,
    ):
```

And change the Redis client init (around line 61-63) from:

```python
        # Redis client for reading app-created users
        self.redis_client = redis_lib.Redis(
            host="localhost", port=6379, db=0, decode_responses=True
        )
```

To:

```python
        # Redis client for reading app-created users (optional)
        self.redis_client = None
        if queue_suffix:
            self.redis_client = redis_lib.Redis(
                host="localhost", port=6379, db=0, decode_responses=True
            )
```

Also guard the Redis read in `_get_redis_users()` (line 152-162):

```python
    def _get_redis_users(self) -> list[str]:
        """Fetch app-created emails from Redis Hash redis-users-{N}."""
        if not self.redis_client:
            return []
        queue_name = f"redis-users-{self.queue_suffix}"
        try:
            data = self.redis_client.hgetall(queue_name)
            emails = [email.lower() for email in data.keys()]
            logger.info("Got %d emails from Redis '%s'", len(emails), queue_name)
            return emails
        except Exception as e:
            logger.error("Failed to read Redis '%s': %s", queue_name, e)
            return []
```

And guard the Redis cleanup in `run()` (around line 413-419):

```python
        if self.deleted > 0 and self.redis_client:
            queue_name = f"redis-users-{self.queue_suffix}"
```

- [ ] **Step 2: Verify syntax**

Run: `cd Manage_User && py -m py_compile deleter.py`
Expected: No output (success)

- [ ] **Step 3: Commit**

```bash
git add Manage_User/deleter.py
git commit -m "refactor(deleter): make Redis optional for API use case"
```

---

## Task 6: Go — Create API client module

**Files:**
- Create: `Get_Profile/token/api.go`

- [ ] **Step 1: Create `api.go` — HTTP client for Python API**

```go
package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	apiBatchDeleteSize = 20
	apiFlushInterval   = 5 * time.Second
)

// APIClient communicates with the Python API service for token
// acquisition and user deletion.
type APIClient struct {
	baseURL    string
	httpClient *http.Client
	deleteChan chan string
}

// tokenResponse is the JSON shape returned by GET /tokens/next (200).
type tokenResponse struct {
	Email        string `json:"email"`
	RefreshToken string `json:"refresh_token"`
	TenantID     string `json:"tenant_id"`
}

// NewAPIClient creates a client pointing at the Python API service.
func NewAPIClient(baseURL string) *APIClient {
	return &APIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		deleteChan: make(chan string, 1000),
	}
}

// FetchToken calls GET /tokens/next.
// Returns a new TokenInfo on 200 or nil when the queue is empty (202).
// Retries on connection errors up to 3 times.
func (c *APIClient) FetchToken() (*TokenInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.httpClient.Get(c.baseURL + "/tokens/next")
		if err != nil {
			lastErr = err
			log.Printf("[API] GET /tokens/next failed (attempt %d): %v", attempt+1, err)
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 200 {
			var tr tokenResponse
			if err := json.Unmarshal(body, &tr); err != nil {
				return nil, fmt.Errorf("parse token response: %w", err)
			}
			return &TokenInfo{
				Username:     tr.Email,
				RefreshToken: tr.RefreshToken,
				TenantID:     tr.TenantID,
			}, nil
		}

		if resp.StatusCode == 202 {
			return nil, nil // Queue empty, caller should retry later
		}

		lastErr = fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
	return nil, fmt.Errorf("fetch token failed after 3 attempts: %w", lastErr)
}

// QueueDelete sends an email to the delete buffer.
// The background goroutine batches and sends DELETE requests.
func (c *APIClient) QueueDelete(email string) {
	select {
	case c.deleteChan <- email:
	default:
		log.Printf("[API] Delete channel full, dropping: %s", email)
	}
}

// StartDeleteWorker launches a goroutine that batches emails and calls
// POST /users/delete. It flushes at apiBatchDeleteSize items or every
// apiFlushInterval. Cancel ctx to stop; remaining items are flushed.
func (c *APIClient) StartDeleteWorker(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		batch := make([]string, 0, apiBatchDeleteSize)
		ticker := time.NewTicker(apiFlushInterval)
		defer ticker.Stop()

		flush := func() {
			if len(batch) == 0 {
				return
			}
			c.sendDeleteBatch(batch)
			batch = batch[:0]
		}

		for {
			select {
			case email, ok := <-c.deleteChan:
				if !ok {
					flush()
					return
				}
				batch = append(batch, email)
				if len(batch) >= apiBatchDeleteSize {
					flush()
				}
			case <-ticker.C:
				flush()
			case <-ctx.Done():
				// Drain remaining
				for {
					select {
					case email, ok := <-c.deleteChan:
						if !ok {
							flush()
							return
						}
						batch = append(batch, email)
						if len(batch) >= apiBatchDeleteSize {
							flush()
						}
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

// CloseDeleteChan closes the delete channel. Call after all workers are done.
func (c *APIClient) CloseDeleteChan() {
	close(c.deleteChan)
}

func (c *APIClient) sendDeleteBatch(emails []string) {
	payload := fmt.Sprintf(`{"emails":[%s]}`, joinQuoted(emails))
	resp, err := c.httpClient.Post(
		c.baseURL+"/users/delete",
		"application/json",
		strings.NewReader(payload),
	)
	if err != nil {
		log.Printf("[API] POST /users/delete failed: %v (batch: %d emails)", err, len(emails))
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 {
		log.Printf("[API] Deleted batch: %s", string(body))
	} else {
		log.Printf("[API] Delete batch failed: %d - %s", resp.StatusCode, string(body))
	}
}

func joinQuoted(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = `"` + s + `"`
	}
	return strings.Join(quoted, ",")
}
```

- [ ] **Step 2: Verify compilation**

Run: `cd Get_Profile && go build ./token/`
Expected: Build succeeds

- [ ] **Step 3: Commit**

```bash
git add Get_Profile/token/api.go
git commit -m "feat(token): API client for Python service (fetch token, batch delete)"
```

---

## Task 7: Go — Modify TokenManager for lazy loading

**Files:**
- Modify: `Get_Profile/token/manager.go`

TokenManager needs a new method to add tokens one-at-a-time (from API) instead of loading all at once.

- [ ] **Step 1: Add `AddToken()` method to Manager**

Add this method after `LoadFromSlice()` (after line 108):

```go
// AddToken adds a single token to the manager and pushes it to the queue
// if queue mode is active. Used for lazy loading from API.
func (m *Manager) AddToken(t *TokenInfo) {
	m.tokenInfos = append(m.tokenInfos, t)
	atomic.AddInt32(&m.totalTokens, 1)
	if m.queueMode && m.tokenQueue != nil {
		select {
		case m.tokenQueue <- t:
		default:
		}
	}
}
```

- [ ] **Step 2: Add `InitEmptyQueue()` for starting with empty pool**

Add after `InitQueue()` (after line 444):

```go
// InitEmptyQueue initializes queue mode with an empty queue.
// Tokens are added later via AddToken().
func (m *Manager) InitEmptyQueue(bufferSize int) {
	m.tokenQueue = make(chan *TokenInfo, bufferSize)
	m.queueMode = true
}
```

- [ ] **Step 3: Verify compilation**

Run: `cd Get_Profile && go build ./token/`
Expected: Build succeeds

- [ ] **Step 4: Commit**

```bash
git add Get_Profile/token/manager.go
git commit -m "feat(token): add AddToken and InitEmptyQueue for lazy API loading"
```

---

## Task 8: Go — Modify config to replace Redis with API

**Files:**
- Modify: `Get_Profile/config/config.go`

- [ ] **Step 1: Replace Redis fields with APIAddr**

Change `config.go`:

Replace:
```go
	// Redis settings
	RedisAddr  string
	RedisQueue string  // Queue suffix
```

With:
```go
	// API settings
	APIAddr string // Python API service address
```

And in `NewConfig()`, replace:
```go
		RedisAddr:        "localhost:6379",
```

With:
```go
		APIAddr:          "http://localhost:5000",
```

Remove the `RedisQueue` default if present.

- [ ] **Step 2: Verify compilation**

Run: `cd Get_Profile && go build ./config/`
Expected: Build succeeds

- [ ] **Step 3: Commit**

```bash
git add Get_Profile/config/config.go
git commit -m "refactor(config): replace Redis fields with APIAddr"
```

---

## Task 9: Go — Delete redis.go and remove go-redis dependency

**Files:**
- Delete: `Get_Profile/token/redis.go`
- Modify: `Get_Profile/go.mod`

- [ ] **Step 1: Delete redis.go**

Delete the file `Get_Profile/token/redis.go`.

- [ ] **Step 2: Remove go-redis dependency**

Run: `cd Get_Profile && go mod tidy`

This will remove `github.com/redis/go-redis/v9` and its indirect dependencies from `go.mod` and `go.sum`.

- [ ] **Step 3: Verify build**

Run: `cd Get_Profile && go build ./...`
Expected: Will fail because main.go still references Redis. That's expected — we fix main.go in the next task.

- [ ] **Step 4: Commit**

```bash
git add Get_Profile/token/redis.go Get_Profile/go.mod Get_Profile/go.sum
git commit -m "refactor(token): remove redis.go and go-redis dependency"
```

Note: We use `git add` on the deleted file path — git tracks the deletion.

---

## Task 10: Go — Rewrite main.go to use API client

**Files:**
- Modify: `Get_Profile/main.go`

- [ ] **Step 1: Rewrite main.go**

Replace the entire `main.go` with the version below. Key changes:
- Remove Redis imports and usage
- Add `--api` flag, remove `-queue`, `-redis` flags
- Use `APIClient` for token loading and deletion
- Pre-fetch initial tokens before starting workers
- Workers call `APIClient.FetchToken()` when pool runs out

```go
//go:build !hot
// +build !hot

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"linkedin_fetcher/api"
	"linkedin_fetcher/config"
	"linkedin_fetcher/reader"
	"linkedin_fetcher/token"
	"linkedin_fetcher/worker"
	"linkedin_fetcher/writer"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	startTimestamp := time.Now().Format("2006-01-02_15-04-05")

	// Parse command line arguments
	emailsFile := flag.String("emails", "", "Path to emails file (default: emails.txt)")
	resultFile := flag.String("result", "", "Path to result file (will use timestamp if not specified)")
	apiAddr := flag.String("api", "http://localhost:5000", "Python API service address")
	numWorkers := flag.Int("workers", 550, "Number of workers")
	instanceID := flag.String("id", "", "Instance ID for logging (optional)")
	maxCPM := flag.Int("max-cpm", 0, "Max requests per minute (0 = use default 20000)")
	flag.Parse()

	// Generate timestamped filenames
	logFileName := fmt.Sprintf("output_%s.log", startTimestamp)
	resultFileName := fmt.Sprintf("result_%s.txt", startTimestamp)
	if *resultFile != "" {
		resultFileName = *resultFile
	}

	// Set up logging to file
	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if *instanceID != "" {
		log.SetPrefix(fmt.Sprintf("[%s] ", *instanceID))
	}
	log.Println("=== LinkedIn Profile Fetcher ===")
	log.Printf("Run timestamp: %s", startTimestamp)
	log.Printf("Log file: %s", logFileName)
	log.Printf("Result file: %s", resultFileName)
	log.Println("Starting application...")

	// Load configuration
	cfg := config.NewConfig()
	cfg.NumWorkers = *numWorkers
	cfg.MaxCPM = 20000 // Hard limit: 20K CPM
	cfg.APIAddr = *apiAddr
	if *emailsFile != "" {
		cfg.EmailsFile = *emailsFile
	}
	cfg.ResultsFile = resultFileName
	if *maxCPM > 0 {
		cfg.MaxCPM = *maxCPM
	}

	log.Printf("[CONFIG] Workers: %d | APITimeout: %ds | MaxCPM: %d",
		cfg.NumWorkers, cfg.APITimeout, cfg.MaxCPM)
	log.Printf("[CONFIG] API: %s", cfg.APIAddr)
	log.Printf("[FILES] Emails: %s | Result: %s", cfg.EmailsFile, cfg.ResultsFile)

	// Check if emails file exists
	if _, err := os.Stat(cfg.EmailsFile); os.IsNotExist(err) {
		log.Fatalf("[ERROR] Emails file not found: %s", cfg.EmailsFile)
	}

	// Create API client
	apiClient := token.NewAPIClient(cfg.APIAddr)

	// Pre-fetch initial tokens from Python API
	tokenManager := token.NewManager()
	tokenManager.InitEmptyQueue(2000)

	log.Println("[TOKEN] Fetching initial tokens from API...")
	initialCount := 0
	for initialCount < 50 {
		t, err := apiClient.FetchToken()
		if err != nil {
			log.Printf("[TOKEN] Fetch error: %v", err)
			break
		}
		if t == nil {
			// Queue empty, wait and retry
			log.Printf("[TOKEN] API queue empty, waiting 2s... (got %d so far)", initialCount)
			time.Sleep(2 * time.Second)
			continue
		}
		tokenManager.AddToken(t)
		initialCount++
	}
	log.Printf("[TOKEN] Pre-fetched %d tokens", initialCount)

	if initialCount == 0 {
		log.Fatal("[ERROR] No tokens available from API, is the Python service running?")
	}

	// Set up dead token channel → API delete
	deadChan := make(chan string, 1000)
	tokenManager.SetDeadChan(deadChan)

	// Start delete worker (batches dead token emails → POST /users/delete)
	deleteCtx, deleteCancel := context.WithCancel(context.Background())
	var deleteWg sync.WaitGroup

	// Bridge deadChan → apiClient.deleteChan
	deleteWg.Add(1)
	go func() {
		defer deleteWg.Done()
		for email := range deadChan {
			apiClient.QueueDelete(email)
		}
	}()
	apiClient.StartDeleteWorker(deleteCtx, &deleteWg)

	// Start background token fetcher (refills pool when low)
	fetchCtx, fetchCancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-fetchCtx.Done():
				return
			default:
			}
			if tokenManager.QueueLen() < 50 {
				t, err := apiClient.FetchToken()
				if err != nil {
					log.Printf("[TOKEN] Background fetch error: %v", err)
					time.Sleep(5 * time.Second)
					continue
				}
				if t == nil {
					time.Sleep(2 * time.Second)
					continue
				}
				tokenManager.AddToken(t)
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}()

	// Initialize email reader
	emailReader := reader.NewEmailReader(cfg.EmailsFile, cfg.FileBufferSize)
	totalEmails := 0

	// Initialize result writer
	resultWriter, err := writer.NewResultWriter(cfg.ResultsFile)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create result writer: %v", err)
	}
	defer resultWriter.Close()

	// Initialize API client (Loki)
	lokiClient := api.NewClient(cfg.APITimeout)

	// Initialize worker pool
	pool := worker.NewPool(cfg.NumWorkers, lokiClient, tokenManager, resultWriter, cfg.EmailBufferSize, cfg.MaxCPM)
	pool.StartProgressReporter(5*time.Second, totalEmails)
	pool.Start()

	// Set up graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start reading emails
	startTime := time.Now()
	emailChan, errChan := emailReader.ReadEmails()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() {
		closeOnce.Do(func() {
			close(done)
		})
	}

	go func() {
		for email := range emailChan {
			pool.Submit(email)
		}
		if err := <-errChan; err != nil {
			log.Printf("[ERROR] Failed to read emails: %v", err)
		}
		pool.Close()
		closeDone()
	}()

	go func() {
		<-sigChan
		log.Println("\n[SHUTDOWN] Received interrupt signal, shutting down...")
		pool.Shutdown()
		closeDone()
	}()

	<-done
	time.Sleep(500 * time.Millisecond)

	// Shutdown cleanup
	fetchCancel()
	close(deadChan)
	time.Sleep(500 * time.Millisecond)
	apiClient.CloseDeleteChan()
	deleteCancel()
	deleteWg.Wait()

	// Print final statistics
	elapsed := time.Since(startTime)
	processed, successful, failed, exactMatch := pool.Stats()

	fmt.Println(repeatString("=", 60))
	log.Println("=== FINAL STATISTICS ===")

	if pool.StoppedEarly() {
		log.Printf("Stop Reason:     %s", pool.StopReason())
	} else {
		log.Printf("Stop Reason:     completed (all emails processed)")
	}

	log.Printf("Total Emails:    %d", totalEmails)
	log.Printf("Processed:       %d", processed)
	log.Printf("Successful:      %d", successful)
	log.Printf("Failed:          %d", failed)
	log.Printf("ExactMatch:      %d", exactMatch)
	log.Printf("Results Written: %d", resultWriter.Count())
	log.Printf("Elapsed Time:    %s", elapsed.Round(time.Second))
	if elapsed.Seconds() > 0 {
		log.Printf("Rate:            %.1f emails/second", float64(processed)/elapsed.Seconds())
	}

	tTotal, tAlive, tDead := tokenManager.Stats()
	log.Printf("Tokens:          %d total, %d alive, %d dead", tTotal, tAlive, tDead)

	fmt.Println(repeatString("=", 60))
	log.Printf("Results saved to: %s", cfg.ResultsFile)

	if pool.StoppedEarly() {
		log.Printf("Application stopped early: %s", pool.StopReason())
	} else {
		log.Println("Application finished successfully.")
	}
}

func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
```

- [ ] **Step 2: Build and verify**

Run: `cd Get_Profile && go build -o get_profile.exe .`
Expected: Build succeeds with no errors

- [ ] **Step 3: Run `go vet`**

Run: `cd Get_Profile && go vet ./...`
Expected: No issues

- [ ] **Step 4: Commit**

```bash
git add Get_Profile/main.go
git commit -m "feat(main): replace Redis with Python API client"
```

---

## Task 11: Go — Clean up go.mod

**Files:**
- Modify: `Get_Profile/go.mod`, `Get_Profile/go.sum`

- [ ] **Step 1: Tidy dependencies**

Run: `cd Get_Profile && go mod tidy`

This should remove `github.com/redis/go-redis/v9` and its indirect dependencies.

- [ ] **Step 2: Verify build**

Run: `cd Get_Profile && go build -o get_profile.exe . && go vet ./...`
Expected: Clean build, no vet warnings

- [ ] **Step 3: Commit**

```bash
git add Get_Profile/go.mod Get_Profile/go.sum
git commit -m "chore(deps): remove go-redis, tidy go.mod"
```

---

## Task 12: End-to-end verification

- [ ] **Step 1: Start Python API service**

Run: `cd Manage_User && py app.py --port 5000`

Expected:
- Logs: `Startup Cleanup: deleting bot_ users`
- Logs: `Producer started`
- Logs: `API service ready on 0.0.0.0:5000`
- Producer starts creating users and filling queue

- [ ] **Step 2: Check /status endpoint (in new terminal)**

Run: `curl http://localhost:5000/status`

Expected: JSON with `queue_size` increasing over time

- [ ] **Step 3: Test /tokens/next**

Run: `curl http://localhost:5000/tokens/next`

Expected: JSON with `email`, `refresh_token`, `tenant_id` (200) or `waiting: true` (202)

- [ ] **Step 4: Build and run Go app**

Run: `cd Get_Profile && go build -o get_profile.exe . && ./get_profile.exe --api http://localhost:5000 --emails emails.txt --workers 550`

Expected:
- Logs: `Pre-fetched N tokens`
- Workers start processing emails
- Dead tokens trigger `POST /users/delete`
- Results written to `result_TIMESTAMP.txt`

- [ ] **Step 5: Verify cleanup after Ctrl+C**

Press Ctrl+C on Go app.
Expected:
- Delete buffer flushed
- Clean shutdown

- [ ] **Step 6: Commit all remaining changes**

```bash
git add -A
git commit -m "feat: merge modules — Python API service + Go app, remove Redis dependency"
```

---

## Summary of Changes

| Area | Before | After |
|------|--------|-------|
| Communication | Redis Hash queues | HTTP API localhost:5000 |
| User naming | `nguyen.van1234@domain` | `bot_a3kx9m@domain` |
| Token loading | HGETALL at startup | `GET /tokens/next` on-demand |
| Dead token cleanup | HDEL via channel → Redis | Batch `POST /users/delete` → Graph API |
| Startup wait | ~40 min (create all + get all tokens) | ~25s (first batch ready) |
| Crash recovery | Redis data must survive | `bot_` prefix cleanup on restart |
| Dependencies | Python, Go, Redis | Python, Go |
