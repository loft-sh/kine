#!/usr/bin/env python3
"""Decide whether a fork release is the highest stable remote tag."""
import os
import re
import subprocess

TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.vcluster\.(0|[1-9][0-9]*)\Z")


def version(tag):
    match = TAG.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None


def promote(tag, refs):
    current = version(tag)
    if current is None:
        return False
    tags = [line.split("\t", 1)[1].removeprefix("refs/tags/") for line in refs.splitlines()]
    if tag not in tags:
        raise ValueError("release tag is missing from remote refs")
    return all(candidate is None or candidate <= current for candidate in map(version, tags))


def main():
    refs = subprocess.run(
        ["git", "ls-remote", "--tags", "--refs", "origin", "refs/tags/*"],
        check=True, capture_output=True, text=True,
    ).stdout
    result = promote(os.environ["TAG"], refs)
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        output.write(f"latest={str(result).lower()}\n")


if __name__ == "__main__":
    main()
