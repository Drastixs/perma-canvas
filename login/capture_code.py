#!/usr/bin/env python3
"""One-shot Microsoft/Duo login, adapted from mcp-canvas-lms (MIT).
The browser runs only during enrollment; no password or browser profile is saved.
"""
import asyncio
import getpass
import json
import os
import sys
from urllib.parse import parse_qs, urlparse
from playwright.async_api import async_playwright

async def main():
    email = os.environ.get("CANVAS_EMAIL") or input_stderr("University email: ")
    password = os.environ.get("CANVAS_PASSWORD") or getpass.getpass("Password: ")
    authorize_url = os.environ["CANVAS_AUTHORIZE_URL"]
    expected_state = parse_qs(urlparse(authorize_url).query).get("state", [""])[0]
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=os.environ.get("LOGIN_HEADED") != "1")
        try:
            context = await browser.new_context(viewport={"width": 412, "height": 915}, is_mobile=True,
                user_agent="Mozilla/5.0 (Linux; Android 16; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.200 Mobile Safari/537.36")
            done = asyncio.get_running_loop().create_future()
            async def route_handler(route):
                u = urlparse(route.request.url)
                if u.scheme == "https" and u.netloc == "sso.canvaslms.com" and u.path == "/canvas/login":
                    params = parse_qs(u.query)
                    if params.get("code") and params.get("state", [""])[0] == expected_state and not done.done():
                        done.set_result(params["code"][0])
                    await route.fulfill(status=200, content_type="text/plain", body="Authorization captured. You can close this window.")
                elif route.request.resource_type in {"image", "media", "font"}:
                    await route.abort()
                else:
                    await route.continue_()
            await context.route("**/*", route_handler)
            page = await context.new_page()
            await page.goto(authorize_url)
            await page.locator('input[type="email"], input[name="loginfmt"]').first.fill(email, timeout=60000)
            await page.locator('input[type="submit"], button[type="submit"]').first.click()
            await page.locator('input[type="password"]:visible').fill(password, timeout=60000)
            await page.locator('input[type="submit"], button[type="submit"]').first.click()
            password = None
            print("Approve Duo on your phone. Waiting up to 3 minutes…", file=sys.stderr, flush=True)
            async def prompts():
                while not done.done():
                    for selector in ['input[value="No"]', 'button:has-text("No")', 'button:has-text("Authorize")', 'input[value="Authorize"]']:
                        locator = page.locator(selector).first
                        if await locator.is_visible():
                            await locator.click()
                    await asyncio.sleep(1)
            task = asyncio.create_task(prompts())
            try:
                code = await asyncio.wait_for(done, 180)
            finally:
                task.cancel()
            print(json.dumps({"code": code}))
        finally:
            await browser.close()

def input_stderr(prompt):
    print(prompt, file=sys.stderr, end="", flush=True)
    return sys.stdin.readline().strip()

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except Exception as exc:
        print(f"Login failed ({type(exc).__name__}). Try LOGIN_HEADED=1 or LOGIN_BROWSER=manual.", file=sys.stderr)
        sys.exit(1)
