#!/usr/bin/env python3
"""Unmount every mounted Kiro CLI disk image on macOS."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass


MOUNT_LINE = re.compile(
    r"^(?P<device>/dev/disk\d+s\d+) on "
    r"(?P<mount_point>/Volumes/Kiro CLI(?: \d+)?) \("
)


@dataclass(frozen=True)
class KiroMount:
    device: str
    mount_point: str


def find_kiro_mounts() -> list[KiroMount]:
    """Return Kiro CLI volumes, excluding unrelated macOS system mounts."""
    result = subprocess.run(
        ["/sbin/mount"],
        check=True,
        capture_output=True,
        text=True,
    )
    mounts: list[KiroMount] = []

    for line in result.stdout.splitlines():
        match = MOUNT_LINE.match(line)
        if match:
            mounts.append(KiroMount(**match.groupdict()))

    return mounts


def unmount(mount: KiroMount, *, dry_run: bool) -> bool:
    command = ["/usr/sbin/diskutil", "unmount", mount.mount_point]
    print(f"{mount.device}: {mount.mount_point}")

    if dry_run:
        print("  dry-run: unmount 생략")
        return True

    result = subprocess.run(command, text=True, capture_output=True)
    message = (result.stdout or result.stderr).strip()
    if message:
        print(f"  {message}")
    return result.returncode == 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="/Volumes/Kiro CLI, Kiro CLI 1, ... 볼륨을 모두 unmount합니다."
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="대상만 표시하고 실제로 unmount하지 않습니다.",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()

    try:
        mounts = find_kiro_mounts()
    except (OSError, subprocess.CalledProcessError) as error:
        print(f"mount 목록을 읽지 못했습니다: {error}", file=sys.stderr)
        return 2

    if not mounts:
        print("마운트된 Kiro CLI 볼륨이 없습니다.")
        return 0

    failed = [mount for mount in mounts if not unmount(mount, dry_run=args.dry_run)]
    if failed:
        print(
            f"{len(failed)}개 볼륨을 unmount하지 못했습니다. 사용 중인 Kiro CLI를 종료한 뒤 다시 시도하세요.",
            file=sys.stderr,
        )
        return 1

    if not args.dry_run:
        print(f"Kiro CLI 볼륨 {len(mounts)}개를 unmount했습니다.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
