#!/usr/bin/env python3
"""
代理池工具：从站大爷免费 API 拉取代理 → 验证可用 → 输出到文件
配合 batch_register.py --proxy-file 使用

用法:
  1. 先去 https://www.zdaye.com/Users/FreeProxy/ 创建免费应用
  2. 拿到 api_url (完整的提取链接)
  3. python3 proxy_fetcher.py --api-url "http://open.***.com/FreeProxy/Get/?app_id=xxx&akey=xxx&count=50&protocol_type=1&return_type=3"
  4. 会在当前目录生成 proxies.txt
"""

import requests
import argparse
import time
import concurrent.futures
import sys

TARGET = "https://subapi.aigcfast.com/api/status"
TIMEOUT = 8

def fetch_proxies(api_url):
    """从站大爷 API 拉取代理列表"""
    print(f"拉取代理...")
    resp = requests.get(api_url, timeout=15)
    data = resp.json()
    if data.get("code") != "10001":
        print(f"API 错误: {data.get('msg')}")
        return []
    proxy_list = data.get("data", {}).get("proxy_list", [])
    proxies = []
    for p in proxy_list:
        proto = p.get("protocol", "http")
        ip = p["ip"]
        port = p["port"]
        proxies.append(f"{proto}://{ip}:{port}")
    print(f"获取到 {len(proxies)} 个代理")
    return proxies

def test_proxy(proxy_url):
    """测试单个代理是否可用"""
    try:
        resp = requests.get(TARGET, proxies={"http": proxy_url, "https": proxy_url},
                            timeout=TIMEOUT, headers={"User-Agent": "Mozilla/5.0"})
        if resp.status_code == 200:
            return proxy_url, resp.elapsed.total_seconds()
    except:
        pass
    return None, 0

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--api-url", required=True, help="站大爷免费代理 API 完整链接")
    parser.add_argument("-o", "--output", default="proxies.txt", help="输出文件")
    parser.add_argument("-w", "--workers", type=int, default=20, help="并发验证数")
    args = parser.parse_args()

    raw = fetch_proxies(args.api_url)
    if not raw:
        return

    print(f"并发验证 {len(raw)} 个代理 (workers={args.workers})...")
    good = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as ex:
        futures = {ex.submit(test_proxy, p): p for p in raw}
        for i, f in enumerate(concurrent.futures.as_completed(futures)):
            proxy, elapsed = f.result()
            if proxy:
                good.append(proxy)
                print(f"  [{len(good)}] {proxy} ({elapsed:.1f}s)")
            if (i + 1) % 20 == 0:
                print(f"  进度: {i+1}/{len(raw)}, 可用: {len(good)}")

    if good:
        with open(args.output, "w") as f:
            for p in good:
                f.write(p + "\n")
        print(f"\n✅ {len(good)}/{len(raw)} 个代理可用 → {args.output}")
    else:
        print(f"\n❌ 0/{len(raw)} 个代理可用，免费代理质量太差")

if __name__ == "__main__":
    main()
