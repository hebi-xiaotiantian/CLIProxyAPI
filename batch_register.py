#!/usr/bin/env python3
"""
SubAPI 批量注册 + 自动提 Key (防限流优化版)
网站: https://subapi.aigcfast.com/
"""

import requests
import random
import string
import time
import sys
import json
import argparse
import os

BASE_URL = "https://subapi.aigcfast.com"

FIRST_NAMES = [
    "alex", "jordan", "casey", "morgan", "riley", "taylor", "jamie", "quinn",
    "sam", "max", "lee", "chris", "pat", "drew", "jesse", "skylar",
    "ryan", "emma", "olivia", "noah", "liam", "sophia", "jackson", "aiden",
    "lucas", "logan", "mason", "ethan", "owen", "harper", "ella", "avery",
    "scarlett", "grace", "chloe", "zoey", "mia", "layla", "nora", "lily",
]
LAST_NAMES = [
    "smith", "johnson", "williams", "brown", "jones", "garcia", "miller",
    "davis", "rodriguez", "martinez", "hernandez", "lopez", "gonzalez",
    "wilson", "anderson", "thomas", "taylor", "moore", "jackson", "martin",
    "lee", "perez", "thompson", "white", "harris", "sanchez", "clark",
]
WORDS = [
    "cloud", "wave", "star", "moon", "fire", "storm", "zen", "nova",
    "pixel", "byte", "echo", "flux", "void", "peak", "core", "edge",
    "spark", "drift", "pulse", "glide", "forge", "crane", "bloom",
    "frost", "ember", "shadow", "raven", "phoenix", "tiger", "wolf",
    "hawk", "fox", "bear", "eagle", "lynx", "viper", "cobra",
]
EMAIL_DOMAINS = [
    "gmail.com", "outlook.com", "yahoo.com", "hotmail.com",
    "proton.me", "icloud.com", "mail.com", "zoho.com",
    "aol.com", "live.com", "fastmail.com",
]
USER_AGENTS = [
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:133.0) Gecko/20100101 Firefox/133.0",
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15",
]

# ---------- utils ----------

def random_ua():
    return random.choice(USER_AGENTS)

def random_delay(base, jitter=0.5):
    return base * (1.0 + random.uniform(-jitter, jitter))

def gen_username():
    styles = [
        lambda: random.choice(FIRST_NAMES) + random.choice(LAST_NAMES),
        lambda: random.choice(FIRST_NAMES) + str(random.randint(10, 9999)),
        lambda: random.choice(WORDS) + random.choice(FIRST_NAMES),
        lambda: random.choice(FIRST_NAMES) + "." + random.choice(LAST_NAMES),
        lambda: random.choice(WORDS) + "_" + str(random.randint(10, 999)),
        lambda: random.choice(FIRST_NAMES) + random.choice("abcdefghijklmnopqrstuvwxyz") + str(random.randint(1, 999)),
        lambda: random.choice(WORDS) + random.choice([w for w in WORDS if w != random.choice(WORDS)]),
        lambda: random.choice(FIRST_NAMES) + "_" + random.choice(LAST_NAMES),
    ]
    return random.choice(styles)()

def gen_email(username):
    if random.random() < 0.3:
        username = username + str(random.randint(1, 999))
    return f"{username}@{random.choice(EMAIL_DOMAINS)}"

def gen_password():
    styles = [
        lambda: random.choice(WORDS).capitalize() + str(random.randint(100, 9999)) + "!",
        lambda: random.choice(FIRST_NAMES).capitalize() + "@" + ''.join(random.choices(string.ascii_lowercase, k=4)) + str(random.randint(10, 99)),
        lambda: ''.join(random.choices(string.ascii_lowercase + string.digits, k=10)) + "#" + str(random.randint(100, 999)),
        lambda: random.choice(WORDS) + "_" + random.choice(WORDS) + str(random.randint(10, 99)),
    ]
    return random.choice(styles)()

# ---------- HTTP ----------

class RateLimitState:
    """全局限流状态，自适应调整延迟"""
    def __init__(self, base_delay):
        self.base_delay = base_delay
        self.consecutive_429 = 0
        self.last_429_time = 0

    def hit_429(self):
        now = time.time()
        if now - self.last_429_time < 120:
            self.consecutive_429 += 1
        else:
            self.consecutive_429 = 1
        self.last_429_time = now
        # 每次429延长基础延迟20%
        self.base_delay *= 1.2

    def cool_down(self):
        # 如果超过 5 分钟没遇到 429，逐步恢复
        if time.time() - self.last_429_time > 300 and self.consecutive_429 > 0:
            self.consecutive_429 = 0
            self.base_delay *= 0.9

def request_with_retry(method, url, rate_state, proxy=None, max_retries=3, **kwargs):
    """智能重试：读 Retry-After 头 + 自适应退避 + 代理支持"""
    if proxy:
        kwargs.setdefault("proxies", proxy)
    for attempt in range(max_retries):
        resp = method(url, **kwargs)
        if resp.status_code != 429:
            return resp
        # 优先用服务器返回的 Retry-After
        retry_after = resp.headers.get("Retry-After")
        if retry_after and retry_after.isdigit():
            wait = int(retry_after)
        else:
            wait = (attempt + 1) * 30 + random.randint(5, 20)
        rate_state.hit_429()
        print(f"\n  ⚡限流 等待 {wait}s (第{attempt+1}次)...", end=" ", flush=True)
        time.sleep(wait)
    return resp

# ---------- 代理池 ----------

def load_proxies(filepath):
    """从文件加载代理列表，每行一个: http://user:pass@host:port 或 socks5://host:port"""
    proxies = []
    with open(filepath) as f:
        for line in f:
            line = line.strip()
            if line and not line.startswith("#"):
                proxies.append(line)
    return proxies

class ProxyPool:
    def __init__(self, proxy_list):
        self.proxies = proxy_list
        self.index = 0

    def next(self):
        if not self.proxies:
            return None
        p = self.proxies[self.index % len(self.proxies)]
        self.index += 1
        return {"http": p, "https": p}

    def random(self):
        if not self.proxies:
            return None
        p = random.choice(self.proxies)
        return {"http": p, "https": p}

def api_headers(extra=None):
    h = {
        "User-Agent": random_ua(),
        "Origin": BASE_URL,
        "Referer": f"{BASE_URL}/",
        "Accept": "application/json",
        "Content-Type": "application/json",
    }
    if extra:
        h.update(extra)
    return h

# ---------- API calls ----------

def register(session, username, email, password, rate_state, proxy=None):
    resp = request_with_retry(
        session.post, f"{BASE_URL}/api/user/register", rate_state, proxy,
        json={"username": username, "email": email, "password": password},
        headers=api_headers(), timeout=30,
    )
    if resp.status_code == 200:
        data = resp.json()
        if data.get("success"):
            return True, data
        return False, data
    return False, f"HTTP {resp.status_code}"

def login(session, username, password, rate_state, proxy=None):
    resp = request_with_retry(
        session.post, f"{BASE_URL}/api/user/login", rate_state, proxy,
        json={"username": username, "password": password},
        headers=api_headers(), timeout=30,
    )
    if resp.status_code == 200:
        data = resp.json()
        if data.get("success"):
            return True, data.get("data", {})
        return False, data
    return False, f"HTTP {resp.status_code}"

def list_tokens(session, user_id, rate_state, proxy=None):
    resp = request_with_retry(
        session.get, f"{BASE_URL}/api/token", rate_state, proxy,
        headers=api_headers({"New-Api-User": str(user_id)}), timeout=30,
    )
    if resp.status_code == 200:
        data = resp.json()
        if data.get("success"):
            return True, data.get("data", {}).get("items", [])
    return False, []

def create_token(session, user_id, name, rate_state, proxy=None):
    resp = request_with_retry(
        session.post, f"{BASE_URL}/api/token", rate_state, proxy,
        json={"name": name, "unlimited_quota": True},
        headers=api_headers({"New-Api-User": str(user_id)}), timeout=30,
    )
    if resp.status_code == 200:
        data = resp.json()
        if data.get("success"):
            return True, None
        return False, data.get("message", str(data))
    return False, f"HTTP {resp.status_code}"

def get_token_key(session, user_id, token_id, rate_state, proxy=None):
    resp = request_with_retry(
        session.post, f"{BASE_URL}/api/token/{token_id}/key", rate_state, proxy,
        json={},
        headers=api_headers({"New-Api-User": str(user_id)}), timeout=30,
    )
    if resp.status_code == 200:
        data = resp.json()
        if data.get("success"):
            return True, data.get("data", {}).get("key", "")
    return False, ""

# ---------- main ----------

def main():
    parser = argparse.ArgumentParser(description="SubAPI 批量注册 (防限流优化版)")
    parser.add_argument("-u", "--url", type=str, default="https://subapi.aigcfast.com",
                        help="目标站点 URL (默认: https://subapi.aigcfast.com)")
    parser.add_argument("-n", "--count", type=int, default=5)
    parser.add_argument("-d", "--delay", type=float, default=120.0,
                        help="基础间隔秒数，遇到429会自动延长 (默认: 120)")
    parser.add_argument("-o", "--output", type=str, default="api_keys.txt")
    parser.add_argument("--password", type=str, default=None)
    parser.add_argument("--proxy-file", type=str, default=None,
                        help="代理列表文件，每行一个: http://user:pass@host:port 或 socks5://host:port")
    parser.add_argument("--resume", type=str, default=None,
                        help="断点续传: 指定已有的输出文件路径，跳过已成功的账号")
    args = parser.parse_args()

    global BASE_URL
    BASE_URL = args.url.rstrip("/")

    rate_state = RateLimitState(args.delay)

    # 加载代理池
    proxy_pool = None
    if args.proxy_file:
        proxies = load_proxies(args.proxy_file)
        if proxies:
            proxy_pool = ProxyPool(proxies)
            print(f"代理池: 已加载 {len(proxies)} 个代理")
        else:
            print("警告: 代理文件为空")

    results = []
    success_count = 0
    fail_count = 0

    # 断点续传：读已有文件中的 username
    skip_usernames = set()
    if args.resume and os.path.exists(args.resume):
        with open(args.resume) as f:
            for line in f:
                if line.startswith("#") or "|" not in line:
                    continue
                skip_usernames.add(line.split("|")[0])
        print(f"断点续传: 已跳过 {len(skip_usernames)} 个已完成账号\n")

    # 初始化输出文件
    write_header = not (args.resume and os.path.exists(args.resume))
    mode = "w" if write_header else "a"
    with open(args.output, mode) as f:
        if write_header:
            f.write(f"# SubAPI 批量注册结果\n")
            f.write(f"# 时间: {time.strftime('%Y-%m-%d %H:%M:%S')}\n")
            f.write(f"# 格式: username | email | password | user_id | api_key\n#\n")

    def append_result(r):
        with open(args.output, "a") as f:
            f.write(f"{r['username']}|{r['email']}|{r['password']}|{r['user_id']}|{r['api_key']}\n")
            f.flush()

    targeted = args.count
    i = 0
    while len(results) < targeted:
        username = gen_username()
        if username in skip_usernames:
            continue
        email = gen_email(username)
        password = args.password or gen_password()

        rate_state.cool_down()
        current_delay = max(30, rate_state.base_delay)

        print(f"[{len(results)+1}/{targeted}] {username} | {email} ", end="", flush=True)
        session = requests.Session()
        proxy = proxy_pool.random() if proxy_pool else None

        # Step 1: 注册
        ok, result = register(session, username, email, password, rate_state, proxy)
        if not ok:
            print(f"❌ 注册 {result}")
            fail_count += 1
            i += 1
            time.sleep(random_delay(current_delay))
            continue
        print(".", end="", flush=True)

        # Step 2: 登录 (加随机延迟避免太密集)
        proxy = proxy_pool.random() if proxy_pool else None
        time.sleep(random.uniform(3, 8))
        ok, user_data = login(session, username, password, rate_state, proxy)
        if not ok:
            print(f"❌ 登录 {user_data.get('message', user_data)}")
            fail_count += 1
            i += 1
            time.sleep(random_delay(current_delay))
            continue
        user_id = user_data.get("id")
        print(".", end="", flush=True)

        # Step 3: 获取或创建 token (再加延迟)
        proxy = proxy_pool.random() if proxy_pool else None
        time.sleep(random.uniform(2, 5))
        ok, tokens = list_tokens(session, user_id, rate_state, proxy)
        if not ok:
            print(f"❌ Token列表")
            fail_count += 1
            i += 1
            time.sleep(random_delay(current_delay))
            continue

        token_id = None
        if tokens:
            token_id = tokens[0].get("id")
        else:
            ok, err = create_token(session, user_id, f"{username}-key", rate_state, proxy)
            if not ok:
                print(f"❌ 创建Key {err}")
                fail_count += 1
                i += 1
                time.sleep(random_delay(current_delay))
                continue
            time.sleep(random.uniform(2, 4))
            ok, tokens = list_tokens(session, user_id, rate_state, proxy)
            if ok and tokens:
                token_id = tokens[0].get("id")
            else:
                print(f"❌ 查找Token")
                fail_count += 1
                i += 1
                time.sleep(random_delay(current_delay))
                continue

        # Step 4: 获取密钥明文
        proxy = proxy_pool.random() if proxy_pool else None
        time.sleep(random.uniform(1, 3))
        if token_id:
            ok, key = get_token_key(session, user_id, token_id, rate_state, proxy)
            if ok and key:
                print(f"✅ {key}")
                r = {"username": username, "email": email, "password": password,
                     "user_id": user_id, "api_key": key}
                results.append(r)
                append_result(r)
                success_count += 1
            else:
                print(f"❌ Key获取")
                fail_count += 1
        else:
            print(f"❌ 无Token")
            fail_count += 1

        i += 1
        if len(results) < targeted:
            wait = random_delay(current_delay)
            print(f"  下次间隔 {wait:.0f}s (基础{rate_state.base_delay:.0f}s)")
            time.sleep(wait)

    # 追加纯 key 列表
    if results:
        with open(args.output, "a") as f:
            f.write(f"\n# 纯 API Key 列表\n")
            for r in results:
                f.write(f"{r['api_key']}\n")
        print(f"\n{'='*50}")
        print(f"完成 成功:{success_count} 失败:{fail_count}")
        print(f"文件: {os.path.abspath(args.output)}")
        print(f"{'='*50}")
    else:
        print("无结果")

if __name__ == "__main__":
    main()
