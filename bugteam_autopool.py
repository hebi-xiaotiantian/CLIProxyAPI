#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
BugTeam -> sub2api auto account pool importer.

Pipeline:
  1. (order)   Create a pickup order on BugTeam (idempotent, one pending at a time)
  2. (poll)    Wait for automatic fulfillment
  3. (download) Fetch the Sub2 JSON bundle
  4. (import)  POST it into sub2api's admin account import API
  5. (recover) Optionally claim 401-recovered accounts and import them too

Commands:
  check          Show BugTeam balance and inventory (read-only)
  order          Create an order now and wait for fulfillment + import
  auto           Cron mode: finish a pending order if any, otherwise place a
                 new one per config (safe to run on a timer)
  recover        Claim all claimable 401 recoveries and import them
  status         Show pending order state

Requires only the Python 3 standard library.
"""

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
CONFIG_PATH = os.environ.get("BUGTEAM_CONFIG", os.path.join(SCRIPT_DIR, "config.json"))
STATE_PATH = os.path.join(SCRIPT_DIR, "state.json")
LOG_PATH = os.path.join(SCRIPT_DIR, "bugteam.log")


def log(msg):
    line = "%s %s" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg)
    print(line, flush=True)
    try:
        with open(LOG_PATH, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass


def load_config():
    with open(CONFIG_PATH, "r", encoding="utf-8") as f:
        return json.load(f)


def load_state():
    if os.path.exists(STATE_PATH):
        try:
            with open(STATE_PATH, "r", encoding="utf-8") as f:
                return json.load(f)
        except (OSError, json.JSONDecodeError):
            return {}
    return {}


def save_state(state):
    tmp = STATE_PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(state, f, ensure_ascii=False, indent=2)
    os.replace(tmp, STATE_PATH)
    os.chmod(STATE_PATH, 0o600)


def http_json(method, url, headers=None, body=None, timeout=30, raw=False):
    req = urllib.request.Request(url, method=method)
    req.add_header("Accept", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, data=data, timeout=timeout) as resp:
            payload = resp.read()
            if raw:
                return resp.status, payload, dict(resp.headers)
            return resp.status, json.loads(payload)
    except urllib.error.HTTPError as e:
        try:
            detail = json.loads(e.read().decode("utf-8", "replace"))
        except Exception:
            detail = {"raw": e.read().decode("utf-8", "replace")[:500]}
        return e.code, detail
    except urllib.error.URLError as e:
        return 0, {"error": "network_error", "detail": str(e.reason)}


# ---------------------------------------------------------------------------
# BugTeam client
# ---------------------------------------------------------------------------

def bt_headers(cfg):
    return {"X-Customer-Token": cfg["bugteam_token"]}


def bt_check(cfg):
    status, data = http_json("GET", cfg["base_url"] + "/api/customer/balance", headers=bt_headers(cfg))
    if status != 200:
        return status, data
    balance = data.get("data", data)
    return status, balance


def bt_inventory(cfg, product, quantity):
    url = cfg["base_url"] + "/api/customer/inventory?product=%s&quantity=%d" % (
        urllib.parse.quote(product), quantity)
    status, data = http_json("GET", url, headers=bt_headers(cfg))
    return status, (data.get("data", data) if status == 200 else data)


def bt_create_order(cfg, product, quantity):
    idem = str(uuid.uuid4())
    headers = dict(bt_headers(cfg))
    headers["Idempotency-Key"] = idem
    status, data = http_json("POST", cfg["base_url"] + "/api/customer/pickup/orders",
                             headers=headers, body={"product": product, "quantity": quantity})
    return status, data, idem


def bt_order_status(cfg, order_id):
    status, data = http_json("GET", cfg["base_url"] + "/api/customer/pickup/orders/%s" % order_id,
                             headers=bt_headers(cfg))
    return status, (data.get("data", data) if status == 200 else data)


def bt_download_sub2(cfg, order_id):
    url = cfg["base_url"] + "/api/customer/pickup/orders/%s/download?format=sub2" % order_id
    status, payload, _ = http_json("GET", url, headers=bt_headers(cfg), raw=True)
    if status != 200:
        try:
            return status, json.loads(payload)
        except Exception:
            return status, {"raw": payload[:500]}
    return status, json.loads(payload)


def bt_recoveries(cfg, limit=50):
    url = cfg["base_url"] + "/api/customer/recoveries?state=claimable&limit=%d" % limit
    status, data = http_json("GET", url, headers=bt_headers(cfg))
    return status, (data.get("data", data) if status == 200 else data)


def bt_claim(cfg, recovery_id, ticket):
    headers = dict(bt_headers(cfg))
    headers["X-Recovery-Ticket"] = ticket
    headers["Idempotency-Key"] = str(uuid.uuid4())
    url = cfg["base_url"] + "/api/customer/recoveries/%s/claim" % recovery_id
    status, payload, _ = http_json("POST", url, headers=headers, raw=True)
    if status != 200:
        try:
            return status, json.loads(payload)
        except Exception:
            return status, {"raw": payload[:500]}
    return status, json.loads(payload)


# ---------------------------------------------------------------------------
# sub2api client
# ---------------------------------------------------------------------------

def s2_login(cfg):
    status, data = http_json("POST", cfg["sub2api_base"] + "/api/v1/auth/login",
                             body={"email": cfg["sub2api_email"], "password": cfg["sub2api_password"]})
    if status != 200:
        return None, (status, data)
    return data.get("data", {}).get("access_token"), None


def s2_import(cfg, token, payload):
    headers = {"Authorization": "Bearer %s" % token}
    body = {"data": payload}
    if cfg.get("skip_default_group_bind") is not None:
        body["skip_default_group_bind"] = cfg["skip_default_group_bind"]
    status, data = http_json("POST", cfg["sub2api_base"] + "/api/v1/admin/accounts/data",
                             headers=headers, body=body)
    return status, (data.get("data", data) if status == 200 else data)


def s2_list_accounts(cfg, token, search=None, platform=None):
    params = []
    if search:
        params.append("search=%s" % urllib.parse.quote(search))
    if platform:
        params.append("platform=%s" % urllib.parse.quote(platform))
    url = cfg["sub2api_base"] + "/api/v1/admin/accounts"
    if params:
        url += "?" + "&".join(params)
    status, data = http_json("GET", url, headers={"Authorization": "Bearer %s" % token})
    if status != 200:
        return status, data
    return status, data.get("data", {})


def s2_find_group_id(cfg, token, name):
    status, data = http_json("GET", cfg["sub2api_base"] + "/api/v1/admin/groups",
                             headers={"Authorization": "Bearer %s" % token})
    if status != 200:
        return None
    for g in (data.get("data", {}).get("items") or []):
        if g.get("name") == name:
            return g.get("id")
    return None


def s2_bind_accounts_to_group(cfg, token, account_ids, group_id):
    if not account_ids or not group_id:
        return
    body = {"account_ids": account_ids, "group_ids": [group_id]}
    status, data = http_json("POST", cfg["sub2api_base"] + "/api/v1/admin/accounts/bulk-update",
                             headers={"Authorization": "Bearer %s" % token}, body=body)
    if status != 200:
        log("Group bind FAILED (%d): %s" % (status, json.dumps(data, ensure_ascii=False)[:300]))
        return
    log("Bound %d accounts to group #%d" % (len(account_ids), group_id))


def bind_imported_accounts(cfg, token, bundle):
    """Bind the freshly imported accounts to the configured target group."""
    target = (cfg.get("target_group") or "").strip()
    if not target:
        return
    group_id = s2_find_group_id(cfg, token, target)
    if not group_id:
        log("Target group %r not found on sub2api; skipping group bind" % target)
        return
    platform = (bundle.get("accounts") or [{}])[0].get("platform", "openai")
    account_ids = []
    for acc in (bundle.get("accounts") or []):
        name = (acc.get("name") or "").strip()
        if not name:
            continue
        status, data = s2_list_accounts(cfg, token, search=name, platform=platform)
        if status != 200:
            continue
        for item in (data.get("items") or []):
            if item.get("name") == name and item.get("id"):
                account_ids.append(item["id"])
    if account_ids:
        s2_bind_accounts_to_group(cfg, token, account_ids, group_id)
    else:
        log("No imported accounts found by name for group bind")


def normalize_payload(payload):
    """Best-effort normalization of a supplier bundle into sub2api's DataPayload."""
    accounts = payload.get("accounts") or []
    proxies = payload.get("proxies") or []
    norm_proxies = []
    for item in proxies:
        if isinstance(item, str):
            parsed = urllib.parse.urlsplit(item if "://" in item else "http://" + item)
            norm = {
                "protocol": parsed.scheme or "http",
                "host": parsed.hostname or "",
                "port": parsed.port or (443 if parsed.scheme == "https" else 80),
                "username": parsed.username or "",
                "password": parsed.password or "",
                "status": "active",
            }
            if norm["host"]:
                norm_proxies.append(norm)
            continue
        if isinstance(item, dict):
            norm = dict(item)
            if not norm.get("protocol") and norm.get("proxy"):
                parsed = urllib.parse.urlsplit(norm["proxy"] if "://" in norm["proxy"] else "http://" + norm["proxy"])
                norm["protocol"] = parsed.scheme or "http"
                norm["host"] = parsed.hostname or ""
                norm["port"] = parsed.port or 80
                norm["username"] = parsed.username or ""
                norm["password"] = parsed.password or ""
            norm.setdefault("status", "active")
            if norm.get("host"):
                norm_proxies.append(norm)
    out = {
        "exported_at": payload.get("exported_at") or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "proxies": norm_proxies,
        "accounts": accounts or [],
    }
    if payload.get("type"):
        out["type"] = payload["type"]
    if payload.get("version"):
        out["version"] = payload["version"]
    return out


# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------

def cmd_check(cfg):
    status, balance = bt_check(cfg)
    if status != 200:
        log("BugTeam balance FAILED (%d): %s" % (status, json.dumps(balance, ensure_ascii=False)[:300]))
        return 1
    fen = balance.get("balance_fen", 0)
    log("BugTeam balance: %.2f CNY (held %.2f, available %.2f)" % (
        fen / 100, balance.get("held_fen", 0) / 100, balance.get("available_fen", 0) / 100))
    product = cfg.get("product", "oauth_30d")
    quantity = int(cfg.get("quantity", 1))
    status, inv = bt_inventory(cfg, product, quantity)
    if status != 200:
        log("Inventory FAILED (%d): %s" % (status, json.dumps(inv, ensure_ascii=False)[:300]))
        return 1
    log("Inventory %s x%d: available=%s missing=%s needs_production=%s est_unit=%.2f fen" % (
        product, quantity,
        inv.get("available"), inv.get("missing"), inv.get("needs_production"),
        inv.get("estimated_unit_price_fen", 0) / 100))
    return 0


def finish_pending_order(cfg, state):
    order_id = state.get("order_id")
    if not order_id:
        return False
    status, order = bt_order_status(cfg, order_id)
    log("Order %s status: %s" % (order_id, json.dumps(order, ensure_ascii=False)[:200]))
    st = (order or {}).get("state", "")
    if st in ("completed", "done", "fulfilled"):
        log("Order %s completed, downloading Sub2 bundle..." % order_id)
        status, bundle = bt_download_sub2(cfg, order_id)
        if status != 200:
            log("Download FAILED (%d): %s" % (status, json.dumps(bundle, ensure_ascii=False)[:300]))
            return False
        return import_bundle(cfg, bundle)
    if st in ("cancelled", "canceled", "failed", "expired"):
        log("Order %s ended with state=%s, clearing pending state" % (order_id, st))
        save_state({})
        return False
    log("Order %s still pending (state=%s)" % (order_id, st))
    return False


def import_bundle(cfg, bundle):
    accounts = bundle.get("accounts") or []
    if not accounts:
        log("Bundle has no accounts, nothing to import")
        return False
    payload = normalize_payload(bundle)
    token, err = s2_login(cfg)
    if err:
        log("sub2api login FAILED: %s" % json.dumps(err, ensure_ascii=False)[:300])
        return False
    status, result = s2_import(cfg, token, payload)
    if status != 200:
        log("sub2api import FAILED (%d): %s" % (status, json.dumps(result, ensure_ascii=False)[:400]))
        return False
    log("Imported %d accounts (created=%s failed=%s, proxies created=%s reused=%s)" % (
        len(accounts), result.get("account_created"), result.get("account_failed"),
        result.get("proxy_created"), result.get("proxy_reused")))
    for e in (result.get("errors") or [])[:10]:
        log("  import error: %s %s: %s" % (e.get("kind"), e.get("name"), e.get("message")))
    if int(result.get("account_created") or 0) > 0:
        bind_imported_accounts(cfg, token, bundle)
    return True


def cmd_order(cfg, product=None, quantity=None):
    product = product or cfg.get("product", "oauth_30d")
    quantity = int(quantity or cfg.get("quantity", 1))
    state = load_state()
    if finish_pending_order(cfg, state):
        save_state({})
        return 0
    if state.get("order_id"):
        log("Pending order %s not finished yet, skipping new order" % state["order_id"])
        return 1

    status, inv = bt_inventory(cfg, product, quantity)
    if status != 200:
        log("Inventory check FAILED (%d): %s" % (status, json.dumps(inv, ensure_ascii=False)[:300]))
        return 1
    if int(inv.get("available", 0)) < quantity:
        log("Not enough inventory: available=%s, requested=%d" % (inv.get("available"), quantity))
        return 1

    status, data, idem = bt_create_order(cfg, product, quantity)
    if status not in (200, 201, 202):
        log("Order creation FAILED (%d): %s" % (status, json.dumps(data, ensure_ascii=False)[:300]))
        return 1
    order_id = (data.get("data") or data).get("order_id")
    if not order_id:
        log("Order response missing order_id: %s" % json.dumps(data, ensure_ascii=False)[:300])
        return 1
    log("Order created: %s (idempotency %s)" % (order_id, idem))
    save_state({"order_id": order_id, "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "product": product, "quantity": quantity})

    deadline = time.time() + int(cfg.get("order_timeout_seconds", 1800))
    while time.time() < deadline:
        time.sleep(3)
        if finish_pending_order(cfg, load_state()):
            save_state({})
            return 0
    log("Order %s not fulfilled within timeout" % order_id)
    return 1


def cmd_auto(cfg):
    state = load_state()
    if state.get("order_id"):
        return cmd_order(cfg)
    return cmd_order(cfg)


def cmd_take(cfg, order_id):
    """Pick up an existing order (e.g. placed from the web dashboard) and import it."""
    if not order_id:
        log("take requires an order id")
        return 1
    state = load_state()
    state["order_id"] = order_id
    save_state(state)
    deadline = time.time() + int(cfg.get("order_timeout_seconds", 1800))
    while time.time() < deadline:
        time.sleep(3)
        if finish_pending_order(cfg, load_state()):
            save_state({})
            return 0
    log("Order %s not fulfilled within timeout" % order_id)
    return 1


def cmd_recover(cfg):
    status, data = bt_recoveries(cfg)
    if status != 200:
        log("Recoveries query FAILED (%d): %s" % (status, json.dumps(data, ensure_ascii=False)[:300]))
        return 1
    items = (data or {}).get("recoveries") or []
    if not items:
        log("No claimable recoveries")
        return 0
    imported = 0
    for item in items:
        rid = item.get("recovery_id")
        ticket = item.get("claim_ticket")
        if not rid or not ticket:
            continue
        status, bundle = bt_claim(cfg, rid, ticket)
        if status != 200:
            log("Claim %s FAILED (%d): %s" % (rid, status, json.dumps(bundle, ensure_ascii=False)[:300]))
            continue
        if import_bundle(cfg, bundle):
            imported += 1
        time.sleep(1)
    log("Recovery import done: %d/%d imported" % (imported, len(items)))
    return 0



def cmd_bind(cfg):
    """Bind all currently ungrouped accounts of the target platform to the target group."""
    target = (cfg.get("target_group") or "").strip()
    if not target:
        log("target_group not configured; nothing to bind")
        return 1
    token, err = s2_login(cfg)
    if err:
        log("sub2api login FAILED: %s" % json.dumps(err, ensure_ascii=False)[:300])
        return 1
    group_id = s2_find_group_id(cfg, token, target)
    if not group_id:
        log("Target group %r not found on sub2api" % target)
        return 1
    platform = cfg.get("bind_platform", "openai")
    url = cfg["sub2api_base"] + "/api/v1/admin/accounts?group=ungrouped"
    if platform:
        url += "&platform=%s" % urllib.parse.quote(platform)
    status, data = http_json("GET", url, headers={"Authorization": "Bearer %s" % token})
    if status != 200:
        log("Ungrouped accounts query FAILED (%d): %s" % (status, json.dumps(data, ensure_ascii=False)[:300]))
        return 1
    items = (data.get("data", {}).get("items") or [])
    ids = [a["id"] for a in items if a.get("id")]
    if not ids:
        log("No ungrouped %s accounts to bind" % platform)
        return 0
    s2_bind_accounts_to_group(cfg, token, ids, group_id)
    log("Bind command done: %d accounts -> %s" % (len(ids), target))
    return 0


def main():
    parser = argparse.ArgumentParser(description="BugTeam -> sub2api auto pool importer")
    parser.add_argument("command", choices=["check", "order", "auto", "take", "recover", "bind", "status"])
    parser.add_argument("--product", help="override product (default from config)")
    parser.add_argument("--quantity", type=int, help="override quantity (default from config)")
    parser.add_argument("order_id", nargs="?", help="order id for the take command")
    args = parser.parse_args()
    cfg = load_config()
    if args.command == "check":
        return cmd_check(cfg)
    if args.command == "order":
        return cmd_order(cfg, args.product, args.quantity)
    if args.command == "auto":
        return cmd_auto(cfg)
    if args.command == "take":
        return cmd_take(cfg, args.order_id)
    if args.command == "recover":
        return cmd_recover(cfg)
    if args.command == "bind":
        return cmd_bind(cfg)
    if args.command == "status":
        print(json.dumps(load_state(), ensure_ascii=False, indent=2))
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
