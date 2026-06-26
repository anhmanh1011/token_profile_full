# Multi-VPS — Code Changes Implementation Plan (Plan 1/3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Language skills (CLAUDE.md, mandatory):** Python tasks → apply `python-patterns`, `python-testing`, and `security-review` (license/token code). Go tasks → apply `golang-testing`, `golang-code-style`, `golang-naming`, `golang-error-handling`, `golang-safety`, `golang-concurrency`.

**Goal:** Make license selection configurable per tenant (default **Office 365 A1 for students**) and expose Get_Profile runtime metrics as a node_exporter textfile, so the systemd units in Plan 2 can pass `--license-sku`, `--usage-location`, and `--metrics-file`.

**Architecture:** Python side — a pure `resolve_license_sku()` function holds the selection logic (alias → A1-students-by-part-number → first-available fallback), wired through `creator.py → producer.py → app.py` as constructor params + CLI flags. Go side — a small `metrics` package writes node_exporter textfile format atomically; `main.go` runs a periodic writer goroutine off existing pool/token/bitmap counters.

**Tech Stack:** Python 3 (stdlib `argparse`, `uuid`; pytest + unittest.mock for tests), Go 1.25 (stdlib `os`/`text` + `testing`).

**This plan is self-contained and testable on its own** (unit tests, no live M365/Loki calls). Plans 2 (Ansible/systemd) and 3 (Grafana/Loki/Prometheus) follow.

---

## File Structure

**Python (`Manage_User/`)**
- Modify `creator.py` — add module-level `resolve_license_sku()` + license constants; add `usage_location` param; use it in `_generate_user_data`; resolve preference in `run()`.
- Modify `producer.py` — `TokenProducer` accepts + forwards `license_sku`, `usage_location`.
- Modify `app.py` — extract `build_arg_parser()`, add `--license-sku` / `--usage-location`, thread into `start_service` → `TokenProducer`.
- Create `requirements-dev.txt` — pin `pytest`.
- Create `tests/__init__.py`, `tests/test_license.py`, `tests/test_producer_wiring.py`, `tests/test_app_args.py`.

**Go (`Get_Profile/`)**
- Create `metrics/textfile.go` — `Snapshot` struct + atomic `Write()`.
- Create `metrics/textfile_test.go` — round-trip test.
- Modify `main.go` — add `--metrics-file` flag + periodic writer goroutine.

---

## Task 0: Set up Python test tooling

**Files:**
- Create: `Manage_User/requirements-dev.txt`
- Create: `Manage_User/tests/__init__.py`

- [ ] **Step 1: Create dev requirements**

`Manage_User/requirements-dev.txt`:
```
-r requirements.txt
pytest>=8.0
```

- [ ] **Step 2: Create the tests package marker**

`Manage_User/tests/__init__.py`: (empty file)
```python
```

- [ ] **Step 3: Install dev deps**

Run: `cd Manage_User && python3 -m pip install -r requirements-dev.txt`
Expected: pytest installs successfully (and existing deps already satisfied).

- [ ] **Step 4: Verify pytest runs (collects nothing yet)**

Run: `cd Manage_User && python3 -m pytest tests/ -q`
Expected: `no tests ran` (exit code 5) — confirms pytest is importable.

- [ ] **Step 5: Commit**

```bash
git add Manage_User/requirements-dev.txt Manage_User/tests/__init__.py
git commit -m "test(manage_user): add pytest dev tooling"
```

---

## Task 1: Pure license resolver `resolve_license_sku()`

The core logic, isolated as a pure function so it can be tested without any network. Semantics:

- preference `None` / `""` / `"auto"` → prefer **A1 students** (part number `STANDARDWOFFPACK_STUDENT`); if it has no free seat, fall back to the first SKU with a free seat (logged as a warning).
- preference is a known alias (`"a1-students"`) → same as auto **with** first-available fallback (it is the default).
- preference is a raw GUID (skuId) → use that exact skuId **only** if present with a free seat; otherwise `None` (no cross-product fallback — pinning is intentional).
- preference is any other string → treat as an exact `skuPartNumber`; use it if it has a free seat; otherwise `None`.
- Only SKUs with `prepaidUnits.enabled - consumedUnits > 0` are eligible.

**Files:**
- Modify: `Manage_User/creator.py` (add constants + `resolve_license_sku` near the top, after `WORKERS`/constants block, before `class BulkUserCreator`)
- Test: `Manage_User/tests/test_license.py`

- [ ] **Step 1: Write the failing test**

`Manage_User/tests/test_license.py`:
```python
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd Manage_User && python3 -m pytest tests/test_license.py -v`
Expected: FAIL — `ImportError: cannot import name 'resolve_license_sku' from 'creator'`.

- [ ] **Step 3: Write minimal implementation**

In `Manage_User/creator.py`, add after the constants block (after line `DELAY_BETWEEN_BATCHES = 1.5`) and before `FIRST_NAMES`:
```python
import uuid

# License selection
A1_STUDENTS_PART_NUMBER = "STANDARDWOFFPACK_STUDENT"  # Office 365 A1 for students
LICENSE_ALIASES = {"a1-students": A1_STUDENTS_PART_NUMBER}


def _is_guid(value: str) -> bool:
    try:
        uuid.UUID(value)
        return True
    except (ValueError, AttributeError, TypeError):
        return False


def _eligible_skus(skus: list[dict]) -> list[dict]:
    out = []
    for sku in skus:
        enabled = sku.get("prepaidUnits", {}).get("enabled", 0)
        consumed = sku.get("consumedUnits", 0)
        if enabled - consumed > 0:
            out.append(sku)
    return out


def resolve_license_sku(skus: list[dict], preference: Optional[str]) -> Optional[str]:
    """Resolve a license preference to a concrete skuId given subscribed SKUs.

    See plan/spec for the full semantics. Only SKUs with a free seat
    (``enabled - consumed > 0``) are eligible.
    """
    eligible = _eligible_skus(skus)
    if not eligible:
        return None

    pref = (preference or "auto").strip()
    is_auto = pref.lower() == "auto" or pref.lower() in LICENSE_ALIASES

    if pref.lower() in LICENSE_ALIASES:
        target_part = LICENSE_ALIASES[pref.lower()]
    elif is_auto:
        target_part = A1_STUDENTS_PART_NUMBER
    elif _is_guid(pref):
        for sku in eligible:
            if sku.get("skuId") == pref:
                return pref
        logger.warning("Pinned license skuId %s has no free seat; not assigning", pref[:8])
        return None
    else:
        target_part = pref  # treat as an exact skuPartNumber

    for sku in eligible:
        if sku.get("skuPartNumber") == target_part:
            return sku.get("skuId")

    if is_auto:
        fallback = eligible[0].get("skuId")
        logger.warning(
            "License %s unavailable; falling back to first available SKU %s (%s)",
            target_part, eligible[0].get("skuPartNumber"), (fallback or "")[:8],
        )
        return fallback

    logger.warning("Pinned license part-number %s has no free seat; not assigning", target_part)
    return None
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd Manage_User && python3 -m pytest tests/test_license.py -v`
Expected: PASS (8 passed).

- [ ] **Step 5: Commit**

```bash
git add Manage_User/creator.py Manage_User/tests/test_license.py
git commit -m "feat(creator): pure resolve_license_sku with A1-students default"
```

---

## Task 2: Wire `usage_location` + license preference into `BulkUserCreator`

**Files:**
- Modify: `Manage_User/creator.py` (`__init__`, `_generate_user_data`, `_get_available_license`, `run`)
- Test: `Manage_User/tests/test_license.py` (append)

- [ ] **Step 1: Write the failing test**

Append to `Manage_User/tests/test_license.py`:
```python
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd Manage_User && python3 -m pytest tests/test_license.py -k "usage_location or preference_stored" -v`
Expected: FAIL — `BulkUserCreator.__init__() got an unexpected keyword argument 'usage_location'`.

- [ ] **Step 3: Write minimal implementation**

In `Manage_User/creator.py`, replace the `__init__` signature/body (lines ~47-63) with:
```python
    def __init__(
        self,
        token_mgr: AdminTokenManager,
        count: int = DEFAULT_COUNT,
        license_sku: Optional[str] = None,
        usage_location: str = "US",
    ):
        self.token_mgr = token_mgr
        self.domain = token_mgr.domain
        self.count = count
        self.license_pref = license_sku  # raw preference: alias / guid / part / "auto"
        self.license_sku: Optional[str] = None  # resolved skuId, set in run()
        self.usage_location = usage_location

        # Results
        self.created_users: list[dict] = []
        self.created_count = 0
        self.licensed_count = 0
        self.failed_count = 0
        self.stats_lock = threading.Lock()
```

In `_generate_user_data`, change the hardcoded line:
```python
            "usageLocation": "US",
```
to:
```python
            "usageLocation": self.usage_location,
```

Replace `_get_available_license` (lines ~97-115) with a method that resolves the stored preference:
```python
    def _resolve_license_sku(self) -> Optional[str]:
        token = self._get_token()
        if not token:
            return None
        headers = {"Authorization": f"Bearer {token}"}
        try:
            resp = self.token_mgr.session.get(
                f"{GRAPH_URL}/subscribedSkus", headers=headers, timeout=30
            )
            if resp.status_code == 200:
                return resolve_license_sku(resp.json().get("value", []), self.license_pref)
        except requests.RequestException as e:
            logger.warning("Failed to get licenses: %s", e)
        return None
```

In `run()`, replace the auto-detect block (lines ~369-373):
```python
        # Auto-detect license
        if not self.license_sku:
            self.license_sku = self._get_available_license()
            if self.license_sku:
                logger.info("Auto-detected license: %s", self.license_sku[:8])
```
with:
```python
        # Resolve license preference → concrete skuId
        self.license_sku = self._resolve_license_sku()
        if self.license_sku:
            logger.info("License resolved (pref=%s): skuId=%s", self.license_pref, self.license_sku)
        else:
            logger.warning("No license assigned (pref=%s)", self.license_pref)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd Manage_User && python3 -m pytest tests/test_license.py -v`
Expected: PASS (all, including the 8 from Task 1).

- [ ] **Step 5: Syntax check the module**

Run: `cd Manage_User && python3 -m py_compile creator.py`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add Manage_User/creator.py Manage_User/tests/test_license.py
git commit -m "feat(creator): configurable usage_location + license preference"
```

---

## Task 3: Forward license + usage_location through `TokenProducer`

**Files:**
- Modify: `Manage_User/producer.py` (`__init__`, `_produce_batch`)
- Test: `Manage_User/tests/test_producer_wiring.py`

- [ ] **Step 1: Write the failing test**

`Manage_User/tests/test_producer_wiring.py`:
```python
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd Manage_User && python3 -m pytest tests/test_producer_wiring.py -v`
Expected: FAIL — `TokenProducer.__init__() got an unexpected keyword argument 'license_sku'`.

- [ ] **Step 3: Write minimal implementation**

In `Manage_User/producer.py`, replace `__init__` (lines ~27-38) with:
```python
    def __init__(
        self,
        token_mgr: AdminTokenManager,
        token_queue: queue.Queue,
        license_sku: str | None = None,
        usage_location: str = "US",
    ):
        self.token_mgr = token_mgr
        self.token_queue = token_queue
        self.license_sku = license_sku
        self.usage_location = usage_location
        self.running = False
        self.thread: threading.Thread | None = None

        # Stats
        self.total_created = 0
        self.total_tokens = 0
        self.total_failed_create = 0
        self.total_failed_token = 0
        self.stats_lock = threading.Lock()
```

In `_produce_batch`, replace the creator line (line ~85):
```python
        creator = BulkUserCreator(self.token_mgr, count)
```
with:
```python
        creator = BulkUserCreator(
            self.token_mgr, count,
            license_sku=self.license_sku, usage_location=self.usage_location,
        )
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd Manage_User && python3 -m pytest tests/test_producer_wiring.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add Manage_User/producer.py Manage_User/tests/test_producer_wiring.py
git commit -m "feat(producer): forward license_sku + usage_location to creator"
```

---

## Task 4: Add CLI flags `--license-sku` / `--usage-location` to `app.py`

**Files:**
- Modify: `Manage_User/app.py` (extract `build_arg_parser`, extend `start_service`, pass to `TokenProducer`)
- Test: `Manage_User/tests/test_app_args.py`

- [ ] **Step 1: Write the failing test**

`Manage_User/tests/test_app_args.py`:
```python
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd Manage_User && python3 -m pytest tests/test_app_args.py -v`
Expected: FAIL — `AttributeError: module 'app' has no attribute 'build_arg_parser'`.

- [ ] **Step 3: Write minimal implementation**

In `Manage_User/app.py`, add a `build_arg_parser` function just above the `if __name__ == "__main__":` block (after `start_service`, ~line 313):
```python
def build_arg_parser() -> "argparse.ArgumentParser":
    import argparse

    parser = argparse.ArgumentParser(description="Manage_User API Service")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=5000)
    parser.add_argument(
        "--config",
        default=str(CONFIG_FILE),
        help="Path to this tenant's admin_token JSON file",
    )
    parser.add_argument(
        "--local-ip",
        default=None,
        help="Outbound source/callout IP for this tenant (overrides config local_ip)",
    )
    parser.add_argument(
        "--license-sku",
        default="a1-students",
        help="License preference: alias (a1-students), skuPartNumber, GUID, or 'auto'",
    )
    parser.add_argument(
        "--usage-location",
        default="US",
        help="Two-letter usageLocation for created users (default US)",
    )
    return parser
```

Replace the `if __name__ == "__main__":` block (lines ~316-338) with:
```python
if __name__ == "__main__":
    args = build_arg_parser().parse_args()
    start_service(
        host=args.host,
        port=args.port,
        config_file=Path(args.config),
        local_ip=args.local_ip,
        license_sku=args.license_sku,
        usage_location=args.usage_location,
    )
```

Extend `start_service` signature (line ~243-248) to:
```python
def start_service(
    host: str = "0.0.0.0",
    port: int = 5000,
    config_file: Path = CONFIG_FILE,
    local_ip: Optional[str] = None,
    license_sku: str = "a1-students",
    usage_location: str = "US",
) -> None:
```

And in `start_service`, replace the producer construction (line ~297):
```python
    _producer = TokenProducer(_token_mgr, token_queue)
```
with:
```python
    _producer = TokenProducer(
        _token_mgr, token_queue,
        license_sku=license_sku, usage_location=usage_location,
    )
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd Manage_User && python3 -m pytest tests/test_app_args.py -v`
Expected: PASS.

- [ ] **Step 5: Run the full Python suite + compile check**

Run: `cd Manage_User && python3 -m pytest tests/ -q && python3 -m py_compile app.py producer.py creator.py`
Expected: all tests pass; no compile errors.

- [ ] **Step 6: Commit**

```bash
git add Manage_User/app.py Manage_User/tests/test_app_args.py
git commit -m "feat(app): --license-sku and --usage-location CLI flags"
```

---

## Task 5: Go `metrics` package — atomic node_exporter textfile writer

**Files:**
- Create: `Get_Profile/metrics/textfile.go`
- Test: `Get_Profile/metrics/textfile_test.go`

- [ ] **Step 1: Write the failing test**

`Get_Profile/metrics/textfile_test.go`:
```go
package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteProducesParsableTextfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "getprofile.prom")

	snap := Snapshot{
		Processed: 100, Successful: 80, Failed: 20, ExactMatch: 75,
		TotalTokens: 10, Alive: 6, Dead: 3, Exhausted: 1,
		Done: 100, TotalLines: 500,
	}
	if err := Write(path, "t1", snap); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)

	want := []string{
		`getprofile_processed_total{tenant="t1"} 100`,
		`getprofile_successful_total{tenant="t1"} 80`,
		`getprofile_failed_total{tenant="t1"} 20`,
		`getprofile_tokens_alive{tenant="t1"} 6`,
		`getprofile_lines_done{tenant="t1"} 100`,
		`getprofile_lines_total{tenant="t1"} 500`,
		"# TYPE getprofile_processed_total counter",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestWriteIsAtomicNoTmpLeftBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "getprofile.prom")
	if err := Write(path, "t1", Snapshot{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd Get_Profile && go test ./metrics/`
Expected: FAIL — build error `undefined: Snapshot` / `undefined: Write` (package has no non-test source yet).

- [ ] **Step 3: Write minimal implementation**

`Get_Profile/metrics/textfile.go`:
```go
// Package metrics writes Get_Profile runtime counters to a Prometheus
// node_exporter textfile, consumed by the textfile collector on each VPS.
package metrics

import (
	"fmt"
	"os"
	"strings"
)

// Snapshot is a point-in-time view of the run's counters.
type Snapshot struct {
	Processed   int64
	Successful  int64
	Failed      int64
	ExactMatch  int64
	TotalTokens int64
	Alive       int64
	Dead        int64
	Exhausted   int64
	Done        int64
	TotalLines  int64
}

type metric struct {
	name, help, typ string
	value           int64
}

// Write renders snap to path in node_exporter textfile format, labelled by
// tenant. The write is atomic: content goes to a temp file in the same
// directory, then os.Rename replaces the target.
func Write(path, tenant string, snap Snapshot) error {
	metrics := []metric{
		{"getprofile_processed_total", "Emails processed (terminal outcomes).", "counter", snap.Processed},
		{"getprofile_successful_total", "Successful profile fetches.", "counter", snap.Successful},
		{"getprofile_failed_total", "Failed profile fetches.", "counter", snap.Failed},
		{"getprofile_exact_match_total", "Exact-match profiles written.", "counter", snap.ExactMatch},
		{"getprofile_tokens_total", "Tokens seen total.", "gauge", snap.TotalTokens},
		{"getprofile_tokens_alive", "Tokens currently alive.", "gauge", snap.Alive},
		{"getprofile_tokens_dead", "Tokens marked dead.", "gauge", snap.Dead},
		{"getprofile_tokens_exhausted", "Tokens marked quota-exhausted.", "gauge", snap.Exhausted},
		{"getprofile_lines_done", "Lines marked done in the bitmap.", "gauge", snap.Done},
		{"getprofile_lines_total", "Total lines in the emails file.", "gauge", snap.TotalLines},
	}

	var b strings.Builder
	label := fmt.Sprintf("{tenant=%q}", tenant)
	for _, m := range metrics {
		fmt.Fprintf(&b, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.typ)
		fmt.Fprintf(&b, "%s%s %d\n", m.name, label, m.value)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write temp metrics file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename metrics file: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd Get_Profile && go test ./metrics/ -v`
Expected: PASS (both tests).

- [ ] **Step 5: Vet**

Run: `cd Get_Profile && go vet ./metrics/`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add Get_Profile/metrics/textfile.go Get_Profile/metrics/textfile_test.go
git commit -m "feat(metrics): atomic node_exporter textfile writer"
```

---

## Task 6: Wire `--metrics-file` periodic writer into `main.go`

**Files:**
- Modify: `Get_Profile/main.go` (import, flag, writer goroutine)

> Note: `tokenManager.FullStats()` returns `(total, alive, dead, exhausted int)`; `pool.Stats()` returns `int64`; `bitmap.Done()`/`bitmap.TotalLines()` return `int64`. Cast `int → int64` for the Snapshot.

- [ ] **Step 1: Add the import**

In `Get_Profile/main.go`, add to the import block (after `"linkedin_fetcher/config"`, keeping import order):
```go
	"linkedin_fetcher/metrics"
```

- [ ] **Step 2: Add the flag**

After the `localIP` flag (line ~37), add:
```go
	metricsFile := flag.String("metrics-file", "", "Path to write node_exporter textfile metrics (empty = disabled)")
```

- [ ] **Step 3: Start the writer goroutine**

In `main.go`, immediately after `pool.Start()` (line ~214), insert:
```go
	// Periodic metrics writer (node_exporter textfile). Disabled when empty.
	if *metricsFile != "" {
		tenant := *instanceID
		if tenant == "" {
			tenant = "default"
		}
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-fetchCtx.Done():
					return
				case <-ticker.C:
					processed, successful, failed, exactMatch := pool.Stats()
					tot, alive, dead, exhausted := tokenManager.FullStats()
					snap := metrics.Snapshot{
						Processed:   processed,
						Successful:  successful,
						Failed:      failed,
						ExactMatch:  exactMatch,
						TotalTokens: int64(tot),
						Alive:       int64(alive),
						Dead:        int64(dead),
						Exhausted:   int64(exhausted),
						Done:        bitmap.Done(),
						TotalLines:  bitmap.TotalLines(),
					}
					if err := metrics.Write(*metricsFile, tenant, snap); err != nil {
						log.Printf("[METRICS] write error: %v", err)
					}
				}
			}
		}()
		log.Printf("[METRICS] Writing textfile metrics to %s every 15s (tenant=%s)", *metricsFile, tenant)
	}
```

- [ ] **Step 4: Build and vet**

Run: `cd Get_Profile && go build . && go vet ./...`
Expected: builds clean, no vet output.

- [ ] **Step 5: Run the full Go test suite**

Run: `cd Get_Profile && go test ./...`
Expected: all packages pass (metrics + netbind).

- [ ] **Step 6: Smoke-check the flag is registered**

Run: `cd Get_Profile && ./get_profile --help 2>&1 | grep -- --metrics-file`
Expected: shows the `--metrics-file` flag line.

- [ ] **Step 7: Commit**

```bash
git add Get_Profile/main.go
git commit -m "feat(get_profile): --metrics-file periodic textfile writer"
```

---

## Self-Review

**Spec coverage (spec section 6 — code changes):**
- 6.1 `creator.py` license logic + `usage_location` → Tasks 1, 2. ✅
- 6.2 `producer.py` forwarding → Task 3. ✅
- 6.3 `app.py` flags → Task 4. ✅
- 6.4 Get_Profile `--metrics-file` → Tasks 5, 6. ✅
- Metrics feed Prometheus (spec section 5) → textfile format matches node_exporter collector; scraping config is Plan 3. ✅

**Placeholder scan:** No TBD/TODO; every code step shows full code. ✅

**Type consistency:**
- `resolve_license_sku(skus, preference)` signature identical across Tasks 1–2 and tests. ✅
- `BulkUserCreator(token_mgr, count, license_sku=, usage_location=)` consistent in Tasks 2, 3. ✅
- `TokenProducer(token_mgr, queue, license_sku=, usage_location=)` consistent in Tasks 3, 4. ✅
- Go `metrics.Snapshot` fields used in Task 6 match the struct in Task 5; `int → int64` casts noted for `FullStats()`. ✅
- `metrics.Write(path, tenant, snap)` signature identical in Tasks 5–6. ✅

**Notes for implementer:**
- `creator.py` line numbers are approximate (the module shifts as edits land); match on the shown code, not the line number.
- Run `cd Manage_User && python3 -m pytest tests/ -q` after each Python task to catch regressions early.
```
