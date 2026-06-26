# Dual-tenant deployment (one IP per tenant)

Run **two tenants on one VPS**, each pinned to its own public source IP
("callout IP"): tenant 1 egresses from the VPS IPv4, tenant 2 from the VPS IPv6.
Both `Manage_User` and `Get_Profile` bind their outbound LinkedIn/Loki and
Microsoft Graph/OAuth traffic to the tenant's IP.

This branch keeps the `main` model of **1 process = 1 admin/tenant**, so two
tenants = two independent process sets, one per IP. No external proxy is used;
each tenant dials out directly from its assigned VPS address.

## What binds, what doesn't

| Traffic | Bound to callout IP? |
| --- | --- |
| Manage_User → Microsoft Graph / OAuth (`requests`) | yes |
| Manage_User → Teams browser flow (`curl_cffi`) | yes |
| Get_Profile → Loki (`eur.loki.delve.office.com`) | yes |
| Get_Profile → Microsoft token exchange | yes |
| Get_Profile → local Manage_User API (`127.0.0.1`) | no (stays on loopback) |

The localhost API client is intentionally **not** bound — binding a public IP
would break dialing `127.0.0.1`.

## Prerequisites

1. Both addresses are assigned to the VPS NIC:
   ```bash
   ip -4 addr show     # confirm the IPv4, e.g. 203.0.113.10
   ip -6 addr show     # confirm a routable IPv6, e.g. 2001:db8::20 (not fe80:: link-local)
   ```
2. The destination must be reachable over the chosen address family. Microsoft
   login/Graph support IPv6; verify Loki too before committing a tenant to IPv6:
   ```bash
   curl --interface 2001:db8::20 -sS -o /dev/null -w '%{http_code}\n' \
     https://eur.loki.delve.office.com/
   ```
   If the IPv6 path to Loki fails, put both tenants on separate IPv4 addresses
   instead — the mechanism is identical, only the `local_ip` value changes.

## Configure

Give each tenant its own admin file with a `local_ip` field. Templates:
`Manage_User/admin_token.tenant1.example.json`,
`Manage_User/admin_token.tenant2.example.json`.

```json
[
  {
    "refresh_token": "...",
    "tenant_id": "...",
    "username": "admin@tenant1.example",
    "domain": "tenant1.example",
    "local_ip": "203.0.113.10"
  }
]
```

`local_ip` may be IPv4 or IPv6. Omit it (or pass an empty string) to use the OS
default route. The `--local-ip` CLI flag overrides the file value.

## Run

### Manage_User (one per tenant, distinct ports)

```bash
cd Manage_User
# Tenant 1 — IPv4
python app.py --port 5000 --config admin_token.tenant1.json --local-ip 203.0.113.10
# Tenant 2 — IPv6
python app.py --port 5001 --config admin_token.tenant2.json --local-ip 2001:db8::20
```

`--local-ip` is optional when the config file already carries `local_ip`; pass
it to override or to keep secrets and IPs in separate places.

### Get_Profile (one per tenant, pointed at the matching API port)

```bash
cd Get_Profile
go build -o get_profile .          # Linux VPS: binary name is get_profile (no .exe)

# Tenant 1 — IPv4
./get_profile --api http://localhost:5000 --local-ip 203.0.113.10 \
  --emails emails_tenant1.txt --checkpoint emails_tenant1.txt.ckpt

# Tenant 2 — IPv6
./get_profile --api http://localhost:5001 --local-ip 2001:db8::20 \
  --emails emails_tenant2.txt --checkpoint emails_tenant2.txt.ckpt
```

Use distinct `--emails`/`--checkpoint`/`--result` per tenant so the two runs
never share progress state.

## Verify the binding is live

While a tenant is running, confirm its outbound LinkedIn/Loki sockets carry the
expected source IP:

```bash
ss -tnp | grep get_profile        # local address column = the tenant's callout IP
```
