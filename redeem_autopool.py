#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Redeem 401 找回平台 -> sub2api 自动换号脚本
============================================

流程（与 redeem_api_sdk.py 语义一致，零第三方依赖）:
  1. 从 sub2api JSON 交付文件提取卡密 (card_code)
  2. health_check 自检: 哪些账号 401 失效
  3. batch_reclaim 批量找回(换新号, mode=401)
  4. 轮询直到完成, 下载新凭据
  5. 更新 sub2api 原账号(按 email 匹配, apply-oauth-credentials),
     保留账号 ID 与分组; 找不到则导入为新账号并绑定 target_group

用法:
  python3 redeem_autopool.py check <sub2api.json> [--base URL] [--cards C1 C2]
  python3 redeem_autopool.py reclaim <sub2api.json> [--base URL] [--cards C1 C2]
  python3 redeem_autopool.py download <sub2api.json> [--base URL] [--cards C1 C2]
  python3 redeem_autopool.py cards <sub2api.json>       # 仅显示卡密

配置: 同目录 config.json (sub2api 对接信息), 可用环境变量覆盖:
  REDEEM_BASE_URL / SUB2API_BASE / SUB2API_EMAIL / SUB2API_PASSWORD
"""

import argparse
import fcntl
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
CONFIG_PATH = os.path.join(SCRIPT_DIR, "config.json")
LOG_PATH = os.path.join(SCRIPT_DIR, "redeem.log")
DEFAULT_BASE = "https://30d.team"


def log(msg):
    line = "%s %s" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg)
    print(line, flush=True)
    try:
        with open(LOG_PATH, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass


def load_config():
    cfg = {}
    if os.path.exists(CONFIG_PATH):
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            cfg.update(json.load(f))
    for key, env in (("base_url", "REDEEM_BASE_URL"),
                     ("sub2api_base", "SUB2API_BASE"),
                     ("sub2api_email", "SUB2API_EMAIL"),
                     ("sub2api_password", "SUB2API_PASSWORD")):
        if os.environ.get(env):
            cfg[key] = os.environ[env]
    cfg.setdefault("base_url", DEFAULT_BASE)
    return cfg


# ---------------------------------------------------------------------------
# HTTP helper
# ---------------------------------------------------------------------------

def http_json(method, url, headers=None, body=None, timeout=120, raw=False):
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
                return resp.status, payload
            try:
                return resp.status, json.loads(payload)
            except json.JSONDecodeError:
                return resp.status, {"raw": payload[:500]}
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode("utf-8", "replace"))
        except Exception:
            return e.code, {"raw": e.read().decode("utf-8", "replace")[:500]}
    except urllib.error.URLError as e:
        return 0, {"error": "network_error", "detail": str(e.reason)}


# ---------------------------------------------------------------------------
# 卡密提取 (兼容两种交付格式)
# ---------------------------------------------------------------------------

def extract_card_codes(data):
    """返回卡密列表。优先顶层 card_code; 否则从 account.name 末尾提取。"""
    codes = []
    top = data.get("card_code")
    if isinstance(top, str) and top.strip():
        codes.append(top.strip())
    elif isinstance(top, list):
        codes.extend(str(c) for c in top if str(c).strip())
    if not codes:
        for acc in (data.get("accounts") or []):
            name = (acc.get("name") or "").strip()
            parts = name.rsplit(" ", 1)
            if len(parts) < 2:
                continue
            candidate = parts[-1].strip()
            if re.match(r"^[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9]$", candidate) and "://" not in candidate:
                codes.append(candidate)
    return list(dict.fromkeys(codes))


def extract_card_codes_from_names(names, prefixes=None):
    """Extract card codes from sub2api account names.

    Delivery platforms append the card code as the last space-separated token
    of account.name (e.g. "... team-be3694-54C4496CBCD9"). Strict matching
    requires a known prefix so ordinary name tokens are never mistaken for
    card codes. Returns a deduplicated list.
    """
    if prefixes is None:
        prefixes = ["team-", "RCL-"]
    prefixes = sorted((p or "").strip() for p in prefixes if p and p.strip())
    codes = []
    for name in names or []:
        if not name or not isinstance(name, str):
            continue
        parts = name.strip().rsplit(" ", 1)
        if len(parts) < 2:
            continue
        candidate = parts[-1].strip()
        if not prefixes:
            continue
        if any(candidate.lower().startswith(p.lower()) for p in prefixes):
            if re.match(r"^[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9]$", candidate):
                codes.append(candidate)
    return list(dict.fromkeys(codes))


# ---------------------------------------------------------------------------
# Redeem 平台客户端 (与 SDK 等价)
# ---------------------------------------------------------------------------

STATE_PATH = os.path.join(SCRIPT_DIR, "state.json")
CACHE_TTL_DEFAULT = 24 * 3600


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
    try:
        os.chmod(STATE_PATH, 0o600)
    except OSError:
        pass


def platforms_of(cfg):
    """Return the list of redeem platforms to probe."""
    plats = cfg.get("platforms") or []
    if not plats:
        return [{"name": "default", "base_url": cfg.get("base_url", DEFAULT_BASE)}]
    return plats


def card_belongs_platform(resp):
    """Decide whether a health-check response claims the card.

    A card belongs to a platform when the response is ok and the card is not
    reported as invalid. Network/platform errors (ok missing/False) are
    inconclusive and must not be treated as a match.
    """
    if not isinstance(resp, dict) or not resp.get("ok"):
        return None
    if int(resp.get("invalid_cards", 0) or 0) > 0:
        return False
    if resp.get("total") is None and resp.get("need_reclaim") is None:
        return None
    return True


def cached_platform(cfg, card_code):
    """Return a platform dict from the probe cache, or None."""
    ttl = int(cfg.get("card_platform_cache_ttl_seconds", CACHE_TTL_DEFAULT))
    state = load_state()
    entry = (state.get("card_platforms") or {}).get(card_code)
    if not entry:
        return None
    if time.time() - float(entry.get("ts", 0)) > ttl:
        return None
    for plat in platforms_of(cfg):
        if plat.get("name") == entry.get("platform"):
            return plat
    return None


def cache_platform(card_code, platform_name):
    state = load_state()
    state.setdefault("card_platforms", {})[card_code] = {
        "platform": platform_name, "ts": time.time()}
    save_state(state)


def resolve_card_platform(cfg, card_code):
    """Determine which platform owns a card code.

    Strategy: try the cached platform first; if the cache is missing or stale,
    probe every configured platform with a read-only health check. The first
    platform that claims the card wins and is cached.
    """
    cached = cached_platform(cfg, card_code)
    if cached:
        return cached, False
    for plat in platforms_of(cfg):
        res, cerr = redeem_health_check(plat["base_url"], [card_code])
        if cerr:
            log("Probe %s for %s inconclusive: %s" % (plat.get("name"), card_code, cerr))
            continue
        claim = card_belongs_platform(res)
        if claim:
            cache_platform(card_code, plat.get("name"))
            return plat, True
        if claim is None:
            log("Probe %s for %s inconclusive response, skipping" % (plat.get("name"), card_code))
            continue
        log("Probe %s: card %s not claimed (invalid_cards>0)" % (plat.get("name"), card_code))
    return None, False


def redeem_health_check(base, card_codes):
    status, data = http_json("POST", base + "/api/redeem/reclaim/health-check",
                             body={"card_codes": card_codes})
    if status != 200 or not data.get("ok"):
        return None, data.get("error", "health-check failed (status %d)" % status)
    return data, None


def redeem_batch_cards(base, card_codes, mode="401", query_only=False):
    status, data = http_json("POST", base + "/api/redeem/reclaim/batch-cards",
                             body={"card_codes": card_codes, "mode": mode, "query_only": query_only})
    if status != 200 or not data.get("ok"):
        return None, data.get("error", "batch-cards failed (status %d)" % status)
    return data, None


def redeem_poll_until_done(base, card_codes, interval=12.0, max_wait=900.0):
    started = time.monotonic()
    last = None
    while time.monotonic() - started <= max_wait:
        res, err = redeem_batch_cards(base, card_codes, mode="all", query_only=True)
        if err:
            time.sleep(interval)
            continue
        last = res
        if res.get("queued", 0) == 0 and res.get("already_running", 0) == 0:
            return res, None
        time.sleep(interval)
    return last, "timeout after %ds" % int(max_wait)


def redeem_download(base, order_no, token):
    url = "%s/api/redeem/orders/%s/download?token=%s" % (base, order_no, urllib.parse.quote(token))
    status, payload = http_json("GET", url, raw=True)
    if status != 200:
        return None, "download failed (status %d)" % status
    return payload, None


# ---------------------------------------------------------------------------
# sub2api 客户端
# ---------------------------------------------------------------------------

def s2_login(cfg):
    status, data = http_json("POST", cfg["sub2api_base"] + "/api/v1/auth/login",
                             body={"email": cfg["sub2api_email"], "password": cfg["sub2api_password"]})
    if status != 200:
        return None, json.dumps(data, ensure_ascii=False)[:300]
    return data.get("data", {}).get("access_token"), None


def s2_find_account_by_email(cfg, token, email):
    url = cfg["sub2api_base"] + "/api/v1/admin/accounts?search=%s&platform=openai" % urllib.parse.quote(email)
    status, data = http_json("GET", url, headers={"Authorization": "Bearer %s" % token})
    if status != 200:
        return None
    for item in (data.get("data", {}).get("items") or []):
        cred = item.get("credentials") or {}
        if item.get("email") == email or cred.get("email") == email:
            return item
    return None


def s2_list_pool_accounts(cfg, token, group_id=None, platform="openai"):
    params = []
    if group_id:
        params.append("group=%d" % group_id)
    if platform:
        params.append("platform=%s" % urllib.parse.quote(platform))
    url = cfg["sub2api_base"] + "/api/v1/admin/accounts"
    if params:
        url += "?" + "&".join(params)
    status, data = http_json("GET", url, headers={"Authorization": "Bearer %s" % token})
    if status != 200:
        return None
    return data.get("data", {}).get("items") or []


def s2_find_group_id(cfg, token, name):
    status, data = http_json("GET", cfg["sub2api_base"] + "/api/v1/admin/groups",
                             headers={"Authorization": "Bearer %s" % token})
    if status != 200:
        return None
    for g in (data.get("data", {}).get("items") or []):
        if g.get("name") == name:
            return g.get("id")
    return None


def s2_apply_oauth(cfg, token, account_id, acc):
    body = {"type": "oauth", "credentials": acc.get("credentials") or {}}
    if acc.get("extra"):
        body["extra"] = acc["extra"]
    status, data = http_json("POST", cfg["sub2api_base"] + "/api/v1/admin/accounts/%d/apply-oauth-credentials" % account_id,
                             headers={"Authorization": "Bearer %s" % token}, body=body)
    return status, (data.get("data", data) if status == 200 else data)


def s2_import_new(cfg, token, bundle):
    """新账号导入 (找不到原账号时的兜底): 复用 sub2api 导入接口 + 绑定 target_group。"""
    status, data = http_json("POST", cfg["sub2api_base"] + "/api/v1/admin/accounts/data",
                             headers={"Authorization": "Bearer %s" % token},
                             body={"data": {"exported_at": bundle.get("exported_at") or "",
                                            "proxies": bundle.get("proxies") or [],
                                            "accounts": bundle.get("accounts") or []}})
    if status != 200:
        return False, json.dumps(data, ensure_ascii=False)[:300]
    result = data.get("data", data)
    created = int(result.get("account_created") or 0)
    if created > 0:
        target = (cfg.get("target_group") or "").strip()
        if target:
            gstatus, gdata = http_json("GET", cfg["sub2api_base"] + "/api/v1/admin/groups",
                                       headers={"Authorization": "Bearer %s" % token})
            gid = None
            if gstatus == 200:
                for g in (gdata.get("data", {}).get("items") or []):
                    if g.get("name") == target:
                        gid = g.get("id")
                        break
            if gid:
                ids = []
                for acc in (bundle.get("accounts") or []):
                    email = (acc.get("credentials") or {}).get("email") or ""
                    found = s2_find_account_by_email(cfg, token, email) if email else None
                    if not found:
                        name = (acc.get("name") or "").strip()
                        if name:
                            lst, ldata = http_json("GET", cfg["sub2api_base"] + "/api/v1/admin/accounts?search=%s" % urllib.parse.quote(name),
                                                   headers={"Authorization": "Bearer %s" % token})
                            if lst == 200:
                                for item in (ldata.get("data", {}).get("items") or []):
                                    if item.get("name") == name and item.get("id"):
                                        found = item
                                        break
                    if found and found.get("id"):
                        ids.append(found["id"])
                if ids:
                    http_json("POST", cfg["sub2api_base"] + "/api/v1/admin/accounts/bulk-update",
                              headers={"Authorization": "Bearer %s" % token},
                              body={"account_ids": ids, "group_ids": [gid]})
    return True, result


def parse_downloaded_bundle(raw_bytes):
    """下载的找回凭据可能是 sub2api bundle 或单账号对象, 容错解析。"""
    try:
        data = json.loads(raw_bytes.decode("utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError):
        return None, "downloaded payload is not JSON"
    if data.get("accounts"):
        return data, None
    if data.get("credentials"):
        return {"accounts": [data]}, None
    return None, "unrecognized payload shape: %s" % list(data.keys())


# ---------------------------------------------------------------------------
# 命令
# ---------------------------------------------------------------------------

def load_bundle(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f), None
    except (OSError, json.JSONDecodeError) as e:
        return None, "cannot read %s: %s" % (path, e)


def cmd_cards(path):
    data, err = load_bundle(path)
    if err:
        log(err)
        return 1
    codes = extract_card_codes(data)
    if not codes:
        log("No card codes found in %s" % path)
        return 1
    for c in codes:
        print(c)
    return 0


def cmd_check(cfg, path, extra_cards):
    data, err = load_bundle(path)
    if err:
        log(err)
        return 1
    codes = extract_card_codes(data) + (extra_cards or [])
    if not codes:
        log("No card codes found")
        return 1
    log("Health check for %d card(s): %s" % (len(codes), ", ".join(codes)))
    for code in codes:
        plat, probed = resolve_card_platform(cfg, code)
        if not plat:
            log("Card %s: no platform claimed it; skipping" % code)
            continue
        res, cerr = redeem_health_check(plat["base_url"], [code])
        if cerr:
            log("Health check %s FAILED: %s" % (code, cerr))
            continue
        log("Card %s [%s]: total=%s need_reclaim=%s healthy=%s cannot_reclaim=%s unknown=%s" % (
            code, plat.get("name"), res.get("total"), res.get("need_reclaim"), res.get("healthy"),
            res.get("cannot_reclaim"), res.get("unknown")))
        for cred in (res.get("credentials") or []):
            log("  - %s [%s] http=%s" % (cred.get("email"), cred.get("category"), cred.get("http_status")))
    return 0


def cmd_download(cfg, path, extra_cards, out_dir):
    data, err = load_bundle(path)
    if err:
        log(err)
        return 1
    codes = extract_card_codes(data) + (extra_cards or [])
    if not codes:
        log("No card codes found")
        return 1
    os.makedirs(out_dir, exist_ok=True)
    by_platform = {}
    for code in codes:
        plat, _ = resolve_card_platform(cfg, code)
        if plat:
            by_platform.setdefault(plat["base_url"], {"name": plat.get("name"), "codes": []})
            by_platform[plat["base_url"]]["codes"].append(code)
        else:
            log("Card %s: no platform claimed it; skipping" % code)
    if not by_platform:
        log("No cards claimed by any platform")
        return 1
    saved = 0
    for base, item in by_platform.items():
        log("Polling %s for %s..." % (item["name"], ", ".join(item["codes"])))
        res, cerr = redeem_poll_until_done(base, item["codes"])
        if not res:
            log("Poll FAILED: %s" % cerr)
            continue
        tasks = []
        for card in (res.get("cards") or []):
            for t in (card.get("tasks") or []):
                tasks.append(t)
        for t in tasks:
            if t.get("status") == "done" and t.get("download_token") and t.get("order_no"):
                payload, derr = redeem_download(base, t["order_no"], t["download_token"])
                if payload is None:
                    log("Download %s FAILED: %s" % (t["order_no"], derr))
                    continue
                path_out = os.path.join(out_dir, "%s.json" % t["order_no"])
                with open(path_out, "wb") as f:
                    f.write(payload)
                log("Saved %s (%d bytes)" % (path_out, len(payload)))
                saved += 1
    log("Downloaded %d/%d recovered credentials" % (saved, len(codes)))
    return 0



def reclaim_cards(cfg, codes, platform=None):
    """Core reclaim flow: health check -> batch reclaim -> poll -> download -> update sub2api.

    platform: optional platform dict ({name, base_url}); defaults to cfg base_url.
    """
    base = (platform or {}).get("base_url") or cfg.get("base_url", DEFAULT_BASE)
    if not codes:
        log("No card codes found")
        return 1

    # 1. 自检
    log("Step 1/4 health check [%s]: %s" % ((platform or {}).get("name", "default"), ", ".join(codes)))
    res, cerr = redeem_health_check(base, codes)
    if cerr:
        log("Health check FAILED: %s" % cerr)
        return 1
    need = int(res.get("need_reclaim") or 0)
    log("  need_reclaim=%s healthy=%s total=%s" % (need, res.get("healthy"), res.get("total")))
    if need <= 0:
        log("No 401 accounts to reclaim; all healthy")
        return 0

    # 2. 找回
    log("Step 2/4 submitting reclaim (mode=401)...")
    batch, berr = redeem_batch_cards(base, codes, mode="401")
    if berr:
        log("Reclaim submit FAILED: %s" % berr)
        return 1
    log("  queued=%s already_running=%s done=%s unreclaimable=%s" % (
        batch.get("queued"), batch.get("already_running"), batch.get("done"), batch.get("unreclaimable")))

    # 3. 轮询
    log("Step 3/4 polling until done...")
    done, derr = redeem_poll_until_done(base, codes)
    if not done:
        log("Poll FAILED: %s" % derr)
        return 1
    if derr:
        log("Poll timeout: %s (partial results below)" % derr)
    tasks = []
    for card in (done.get("cards") or []):
        for t in (card.get("tasks") or []):
            tasks.append(t)
    log("  done=%s updated=%s no_action=%s unreclaimable=%s failed=%s" % (
        done.get("done"), done.get("updated"), done.get("no_action"),
        done.get("unreclaimable"), done.get("failed")))

    # 4. 下载并更新
    log("Step 4/4 downloading and updating sub2api accounts...")
    token, lerr = s2_login(cfg)
    if lerr:
        log("sub2api login FAILED: %s" % lerr)
        return 1
    updated = downloaded = failed = 0
    for t in tasks:
        if t.get("status") != "done" or not t.get("download_token") or not t.get("order_no"):
            continue
        payload, derr = redeem_download(base, t["order_no"], t["download_token"])
        if payload is None:
            log("Download %s FAILED: %s" % (t["order_no"], derr))
            failed += 1
            continue
        bundle, perr = parse_downloaded_bundle(payload)
        if perr:
            log("Parse %s FAILED: %s" % (t["order_no"], perr))
            failed += 1
            continue
        downloaded += 1
        accs = bundle.get("accounts") or []
        if not accs:
            log("Order %s returned no accounts" % t["order_no"])
            continue
        acc = accs[0]
        email = (acc.get("credentials") or {}).get("email") or ""
        existing = s2_find_account_by_email(cfg, token, email) if email else None
        if not existing and email:
            # Fallback: match by account name (supplier card code is in the name).
            for item in (s2_list_pool_accounts(cfg, token, platform="openai") or []):
                if item.get("name") == (acc.get("name") or ""):
                    existing = item
                    break
        if existing:
            st, rdata = s2_apply_oauth(cfg, token, existing["id"], acc)
            if st == 200:
                log("Updated account #%s (%s) with recovered credentials" % (existing["id"], email))
                updated += 1
            else:
                log("Update account %s FAILED (%d): %s" % (email, st, json.dumps(rdata, ensure_ascii=False)[:300]))
                failed += 1
        else:
            ok, rdata = s2_import_new(cfg, token, bundle)
            if ok:
                log("Imported new account %s (no existing match): %s" % (email, json.dumps(rdata, ensure_ascii=False)[:200]))
                updated += 1
            else:
                log("Import %s FAILED: %s" % (email, rdata))
                failed += 1
    log("Reclaim complete: downloaded=%d updated=%d failed=%d" % (downloaded, updated, failed))
    return 0 if failed == 0 else 1


def cmd_reclaim(cfg, path, extra_cards):
    data, err = load_bundle(path)
    if err:
        log(err)
        return 1
    codes = extract_card_codes(data) + (extra_cards or [])
    if not codes:
        log("No card codes found")
        return 1
    by_platform = {}
    for code in codes:
        plat, _ = resolve_card_platform(cfg, code)
        if plat:
            by_platform.setdefault(plat["base_url"], {"name": plat.get("name"), "codes": []})
            by_platform[plat["base_url"]]["codes"].append(code)
        else:
            log("Card %s: no platform claimed it; skipping" % code)
    if not by_platform:
        log("No cards claimed by any platform")
        return 1
    rc = 0
    for base, item in by_platform.items():
        plat = {"name": item["name"], "base_url": base}
        rc |= reclaim_cards(cfg, item["codes"], platform=plat)
    return rc



def cmd_auto(cfg, scan_dir):
    """Auto health-check every card found in the sub2api account pool (card
    codes embedded in account names), plus any delivery files in scan_dir.
    Reclaims 401 accounts automatically."""
    token, lerr = s2_login(cfg)
    if lerr:
        log("sub2api login FAILED: %s" % lerr)
        return 1
    codes = []
    # Primary source: account pool of the target group (e.g. codex).
    group_name = (cfg.get("target_group") or "").strip()
    group_id = s2_find_group_id(cfg, token, group_name) if group_name else None
    if group_id:
        items = s2_list_pool_accounts(cfg, token, group_id=group_id)
        log("Pool source: group %r (#%d), %d account(s)" % (group_name, group_id, len(items or [])))
        codes.extend(extract_card_codes_from_names(
            [a.get("name") for a in items],
            prefixes=cfg.get("card_code_prefixes")))
    # Secondary source: delivery files dropped into scan_dir.
    if os.path.isdir(scan_dir):
        files = sorted(f for f in os.listdir(scan_dir)
                       if f.lower().endswith(".json") and f not in ("config.json",))
        for name in files:
            data, err = load_bundle(os.path.join(scan_dir, name))
            if err:
                continue
            codes.extend(extract_card_codes(data))
    codes = list(dict.fromkeys(c for c in codes if c))
    if not codes:
        log("No card codes found (pool group %r or %s)" % (group_name, scan_dir))
        return 0
    log("Auto scan: %d unique card(s): %s" % (len(codes), ", ".join(codes)))
    for code in codes:
        plat, probed = resolve_card_platform(cfg, code)
        if not plat:
            log("Card %s: no platform claimed it; skipping" % code)
            continue
        if probed:
            log("Card %s claimed by platform %s (probed)" % (code, plat.get("name")))
        res, cerr = redeem_health_check(plat["base_url"], [code])
        if cerr:
            log("Health check %s FAILED: %s" % (code, cerr))
            continue
        need = int(res.get("need_reclaim") or 0)
        log("Card %s [%s]: need_reclaim=%s healthy=%s total=%s" % (
            code, plat.get("name"), need, res.get("healthy"), res.get("total")))
        if need > 0:
            log("Card %s has %d 401 account(s), starting reclaim..." % (code, need))
            reclaim_cards(cfg, [code], platform=plat)
        else:
            log("Card %s all healthy, skip" % code)
    log("Auto scan done")
    return 0


def main():
    parser = argparse.ArgumentParser(description="Redeem 401 reclaim -> sub2api auto replace")
    parser.add_argument("command", choices=["cards", "check", "download", "reclaim", "auto"])
    parser.add_argument("json_file", nargs="?", help="sub2api JSON delivery file")
    parser.add_argument("--base", help="redeem platform base URL (default from config)")
    parser.add_argument("--cards", nargs="+", help="extra card codes (if the file has none)")
    parser.add_argument("--out", default="recovered", help="download output dir")
    parser.add_argument("--dir", default=SCRIPT_DIR, help="scan directory for the auto command")
    args = parser.parse_args()
    cfg = load_config()
    if args.base:
        cfg["base_url"] = args.base
    if args.command == "cards":
        if not args.json_file:
            parser.error("json_file required")
        return cmd_cards(args.json_file)
    if args.command == "auto":
        lock_path = os.path.join(SCRIPT_DIR, "auto.lock")
        with open(lock_path, "w") as lock_f:
            try:
                fcntl.flock(lock_f, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                log("Another auto run is in progress, skipping")
                return 0
            try:
                return cmd_auto(cfg, args.dir)
            finally:
                fcntl.flock(lock_f, fcntl.LOCK_UN)
    if not args.json_file:
        parser.error("json_file required")
    if args.command == "check":
        return cmd_check(cfg, args.json_file, args.cards)
    if args.command == "download":
        return cmd_download(cfg, args.json_file, args.cards, args.out)
    if args.command == "reclaim":
        return cmd_reclaim(cfg, args.json_file, args.cards)
    return 1


if __name__ == "__main__":
    sys.exit(main())
