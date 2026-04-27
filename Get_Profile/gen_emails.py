"""Generate 10K fake emails for smoke-testing Get_Profile.

Uses RFC 2606 reserved domains so we don't accidentally probe real users.
"""
import random
from pathlib import Path

random.seed(42)

DOMAINS = ["example.com", "example.net", "example.org", "test.example",
           "invalid.test", "smoke.test", "fake.example"]

FIRST = ["alice", "bob", "carol", "dave", "eve", "frank", "grace", "heidi",
        "ivan", "judy", "ken", "leo", "mallory", "nick", "olivia", "peggy",
        "quinn", "rita", "sam", "trent", "uma", "victor", "walter", "xena",
        "yara", "zane"]
LAST = ["smith", "jones", "doe", "tran", "nguyen", "kim", "garcia", "lee",
       "khan", "brown", "white", "black", "green", "patel", "wilson",
       "miller", "davis", "moore", "taylor", "anderson"]

out = Path(__file__).parent / "emails.txt"
with out.open("w", encoding="utf-8") as f:
    for i in range(10_000):
        u = f"{random.choice(FIRST)}.{random.choice(LAST)}{i}"
        d = random.choice(DOMAINS)
        f.write(f"{u}@{d}\n")

print(f"Wrote {out} ({out.stat().st_size:,} bytes)")
