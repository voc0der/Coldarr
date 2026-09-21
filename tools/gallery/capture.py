#!/usr/bin/env python3
"""Screenshot Coldarr's web GUI, in light and dark, from a running gallery
container.

Usually run through run.sh, which builds and starts the container first.
Scenes run in order because some change state: the apply scene really
moves the plan's items, and the dead-drive scene unmounts a drive for the
rest of the container's life.

Needs Playwright: pip install playwright && playwright install chromium

With Pillow built against libimagequant (most distro packages are; PyPI
wheels aren't), each screenshot is re-encoded as a 256-colour PNG: visually
identical for a UI like this, about a third of the size - which matters for
images committed to the repo. Without it they're kept full-colour.
"""

import argparse
import re
import subprocess
import sys
import time
from pathlib import Path

from playwright.sync_api import Page, sync_playwright

try:
    from PIL import Image, features

    CAN_SHRINK = bool(features.check_feature("libimagequant"))
except ImportError:
    CAN_SHRINK = False

PASSWORD = "gallery"
THEMES = ("light", "dark")

SCENES = [
    "login",
    "dashboard",
    "plan",
    "tiers",
    "tier-edit",
    "connections",
    "scheduler",
    "orphans",
    "applying",
    "history",
    "verify",
    "dead-drive",
]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default="http://127.0.0.1:18478", help="where the gallery container's Coldarr is published")
    ap.add_argument("--out", type=Path, required=True, help="directory to write <scene>-<theme>.png into")
    ap.add_argument("--container", default="coldarr-gallery", help="gallery container name (the dead-drive scene unmounts a drive inside it)")
    ap.add_argument("--only", nargs="+", choices=SCENES, metavar="SCENE", help=f"capture only these scenes: {', '.join(SCENES)}")
    ap.add_argument("--width", type=int, default=1280)
    ap.add_argument("--height", type=int, default=800)
    ap.add_argument("--scale", type=float, default=2, help="device pixel ratio (2 = crisp on high-DPI screens)")
    args = ap.parse_args()

    args.out.mkdir(parents=True, exist_ok=True)
    if not CAN_SHRINK:
        print("note: Pillow with libimagequant not found - keeping full-colour PNGs (about 3x larger)", file=sys.stderr)
    wanted = set(args.only or SCENES)
    problems: list[str] = []

    with sync_playwright() as p:
        browser = p.chromium.launch()
        pages: dict[str, Page] = {}
        for theme in THEMES:
            ctx = browser.new_context(
                viewport={"width": args.width, "height": args.height},
                device_scale_factor=args.scale,
                color_scheme=theme,
            )
            page = ctx.new_page()
            page.on("pageerror", lambda e, t=theme: problems.append(f"[{t}] page error: {e}"))
            page.on("console", lambda m, t=theme: m.type == "error" and problems.append(f"[{t}] console error: {m.text}"))
            pages[theme] = page

        cap = Capturer(args.base, args.out, pages, args.container)

        if "login" in wanted:
            cap.shot("login", "/login")
        for page in pages.values():
            cap.log_in(page)

        for scene in SCENES[1:]:
            if scene in wanted:
                print(f"scene: {scene}", flush=True)
                getattr(cap, scene.replace("-", "_"))()

        browser.close()

    if problems:
        print("\nThe browser reported problems:", file=sys.stderr)
        for line in problems:
            print("  " + line, file=sys.stderr)
        return 1
    print(f"\nwrote {len(list(args.out.glob('*.png')))} screenshots to {args.out}")
    return 0


def shrink(file: Path) -> None:
    """Re-encode as a 256-colour palette PNG (see the module docstring)."""
    with Image.open(file) as im:
        palette = im.convert("RGB").quantize(256, method=Image.Quantize.LIBIMAGEQUANT)
    palette.save(file, optimize=True)


class Capturer:
    def __init__(self, base: str, out: Path, pages: dict[str, Page], container: str):
        self.base, self.out, self.pages, self.container = base.rstrip("/"), out, pages, container

    def shot(self, name: str, path: str | None = None, prepare=None) -> None:
        """Screenshot every theme's page, after loading path (if given) and
        running prepare(page) (if given)."""
        for theme, page in self.pages.items():
            if path is not None:
                page.goto(self.base + path, wait_until="load")
            if prepare:
                prepare(page)
            # No hover highlight from wherever the last click left the
            # pointer, and no focus ring on the last button pressed.
            page.mouse.move(0, 0)
            page.evaluate("document.activeElement && document.activeElement.blur()")
            page.wait_for_timeout(150)  # let fonts/icons settle
            file = self.out / f"{name}-{theme}.png"
            page.screenshot(path=str(file), full_page=True)
            if CAN_SHRINK:
                shrink(file)
            print(f"  {file.name}")

    def log_in(self, page: Page) -> None:
        page.goto(self.base + "/login")
        page.fill("#password", PASSWORD)
        page.click("button[type=submit]")
        page.wait_for_url(self.base + "/")

    def dashboard(self) -> None:
        self.shot("dashboard", "/")

    def plan(self) -> None:
        self.shot("plan", "/plan")

    def tiers(self) -> None:
        self.shot("tiers", "/settings/tiers")

    def tier_edit(self) -> None:
        self.shot("tier-edit", "/settings/tiers/cold-movies/edit")

    def connections(self) -> None:
        def test_all(page: Page) -> None:
            for app in ("radarr", "sonarr", "jellyfin"):
                page.click(f"button[hx-target='#test-{app}']")
                page.wait_for_selector(f"#test-{app} .badge")

        self.shot("connections", "/settings/connections", prepare=test_all)

    def scheduler(self) -> None:
        self.shot("scheduler", "/settings/scheduler")

    def orphans(self) -> None:
        self.shot("orphans", "/settings/orphans")

    def applying(self) -> None:
        """Start the plan, then capture it mid-run with some moves done,
        some moving and some still pending."""
        page = self.pages["light"]
        page.goto(self.base + "/plan")
        page.once("dialog", lambda d: d.accept())
        page.click("text=Apply this plan")
        page.wait_for_url(self.base + "/plan")

        deadline = time.time() + 180
        while time.time() < deadline:
            counts = self.apply_counts()
            if counts["done"] >= 3 and counts["moving"] >= 1 and counts["pending"] >= 2:
                break
            if counts["moving"] == 0 and counts["pending"] == 0:
                print("  warning: the apply finished before a mid-run moment was caught", file=sys.stderr)
                break
            time.sleep(0.5)
        self.shot("applying", "/plan")

        while time.time() < deadline and self.apply_running():
            time.sleep(1)
        if self.apply_running():
            raise SystemExit("the apply did not finish within 3 minutes - see `docker logs " + self.container + "`")

    def apply_counts(self) -> dict[str, int]:
        html = self.pages["light"].request.get(self.base + "/plan/apply/status/partial").text()
        return {s: len(re.findall(rf"</svg> {s}</span>", html)) for s in ("done", "moving", "pending", "failed")}

    def apply_running(self) -> bool:
        html = self.pages["light"].request.get(self.base + "/plan/apply/status/partial").text()
        return "hx-trigger" in html

    def history(self) -> None:
        self.shot("history", "/history")

    def verify(self) -> None:
        page = self.pages["light"]
        page.goto(self.base + "/history")
        page.click("text=Verify sizes on this page")
        page.click("#verify-dialog button[value=quick]")
        page.wait_for_url(re.compile(r".*/history/verify/status.*"))
        page.wait_for_selector("#verify-status:not([hx-trigger])", timeout=60_000)
        self.shot("verify", "/history/verify/status")

    def dead_drive(self) -> None:
        """Pull a satellite drive out from under Coldarr: its tier path is
        left behind as an empty directory on the system disk, as it is in
        Docker when a drive dies."""
        subprocess.run(["docker", "exec", self.container, "umount", "-l", "/mnt/sat2"], check=True)
        self.shot("dead-drive", "/")
        self.shot("dead-drive-plan", "/plan")


if __name__ == "__main__":
    sys.exit(main())
