#!/usr/bin/env python3
"""Changelog fragments (#448): one file per change in changelog.d/, assembled into CHANGELOG.md at release.

Usage (through scripts/changelog.sh):
  check                       validate every fragment; exit 1 with one line per problem
  preview                     print the ## [Unreleased] section as the next release would write it
  release VERSION DATE        write ## [VERSION] - DATE into CHANGELOG.md, move Description parts to
                              docs/changes/VERSION/, delete released and dropped fragments
  unreleased [FILE]           print the ## [Unreleased] section of FILE (default CHANGELOG.md, '-' = stdin); CI uses it

Paths come from the environment, for tests: CHANGELOG_FILE, CHANGELOG_DIR, CHANGES_DIR (defaults relative to the
repository root). Only the Python standard library is used: the frontmatter is a small YAML subset
(key: value, inline lists [a, b], # comments).
"""
import os
import re
import sys

SECTIONS = ["Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"]
TYPES = ["feature", "fix", "task", "security", "docs", "deprecation", "removal"]
STATUSES = ["planned", "in-progress", "done", "dropped"]
KEYS = {"issue", "issues", "type", "section", "status", "breaking", "title", "components", "pr"}
BODY = ["Summary", "Upgrade", "Description"]

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHANGELOG = os.environ.get("CHANGELOG_FILE", os.path.join(ROOT, "CHANGELOG.md"))
FRAG_DIR = os.environ.get("CHANGELOG_DIR", os.path.join(ROOT, "changelog.d"))
CHANGES_DIR = os.environ.get("CHANGES_DIR", os.path.join(ROOT, "docs", "changes"))
UNRELEASED = "## [Unreleased]"


def scalar(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
        return v[1:-1]
    if v in ("true", "false"):
        return v == "true"
    if re.fullmatch(r"-?[0-9]+", v):
        return int(v)
    return v


def strip_comment(v):
    # a '#' starts a comment only after whitespace and outside quotes, so 'title: Fix #12' stays whole if quoted
    out, q = [], None
    for i, c in enumerate(v):
        if q:
            if c == q:
                q = None
        elif c in "\"'":
            q = c
        elif c == "#" and (i == 0 or v[i - 1] in " \t"):
            break
        out.append(c)
    return "".join(out).strip()


def parse_value(v):
    v = strip_comment(v)
    if v.startswith("[") and v.endswith("]"):
        inner = v[1:-1].strip()
        return [scalar(x) for x in inner.split(",")] if inner else []
    return scalar(v)


class Fragment:
    def __init__(self, path):
        self.path = path
        self.name = os.path.basename(path)
        self.meta = {}
        self.body = {}
        self.errors = []
        self._parse()

    def err(self, msg):
        self.errors.append(f"{self.name}: {msg}")

    def _parse(self):
        with open(self.path, encoding="utf-8") as f:
            lines = f.read().splitlines()
        if not lines or lines[0].strip() != "---":
            self.err("must start with a '---' frontmatter line")
            return
        try:
            end = lines.index("---", 1)
        except ValueError:
            self.err("frontmatter has no closing '---'")
            return
        for n, line in enumerate(lines[1:end], 2):
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            m = re.match(r"^([a-z_]+):(.*)$", line)
            if not m:
                self.err(f"line {n}: expected 'key: value', got {line!r}")
                continue
            k = m.group(1)
            if k in self.meta:
                self.err(f"line {n}: duplicate key '{k}'")
            self.meta[k] = parse_value(m.group(2))
        cur = None
        for line in lines[end + 1:]:
            m = re.match(r"^## (.+?)\s*$", line)
            if m:
                cur = m.group(1)
                if cur in self.body:
                    self.err(f"duplicate section '## {cur}'")
                self.body[cur] = []
                continue
            if cur is None:
                if line.strip():
                    self.err("text before the first '## ' section")
                continue
            self.body[cur].append(line)
        for k in self.body:
            self.body[k] = trim(self.body[k])

    @property
    def issues(self):
        v = self.meta.get("issues", self.meta.get("issue"))
        return v if isinstance(v, list) else [v]

    @property
    def status(self):
        return self.meta.get("status")

    def validate(self):
        m = self.meta
        for k in m:
            if k not in KEYS:
                self.err(f"unknown key '{k}' (allowed: {', '.join(sorted(KEYS))})")
        if ("issue" in m) == ("issues" in m):
            self.err("needs exactly one of 'issue' (int) or 'issues' (list of int)")
        elif not self.issues or not all(isinstance(i, int) and not isinstance(i, bool) and i > 0 for i in self.issues):
            self.err("issue/issues must be positive integers")
        for k, allowed in (("type", TYPES), ("section", SECTIONS), ("status", STATUSES)):
            if k not in m:
                self.err(f"missing required key '{k}'")
            elif m[k] not in allowed:
                self.err(f"{k}: '{m[k]}' is not one of {', '.join(allowed)}")
        if "breaking" in m and not isinstance(m["breaking"], bool):
            self.err("breaking must be true or false")
        t = m.get("title")
        if not isinstance(t, str) or not t.strip():
            self.err("missing required key 'title' (one line of text)")
        if "components" in m and not (isinstance(m["components"], list) and all(isinstance(c, str) for c in m["components"])):
            self.err("components must be a list, e.g. [operator, api]")
        if "pr" in m and not (isinstance(m["pr"], int) and m["pr"] > 0):
            self.err("pr must be a positive integer")
        for k in self.body:
            if k not in BODY:
                self.err(f"unknown section '## {k}' (allowed: {', '.join(BODY)})")
        s = self.body.get("Summary", [])
        if not s:
            self.err("missing required '## Summary' section with 1-6 lines")
        elif len(s) > 6:
            self.err(f"'## Summary' has {len(s)} lines, max. 6 (put the rest in '## Description')")
        if m.get("breaking") is True and not self.body.get("Upgrade"):
            self.err("breaking: true needs a '## Upgrade' section with the migration steps")
        if not self.errors:
            want = re.match(r"^([0-9]+)-[a-z0-9][a-z0-9-]*\.md$", self.name)
            if not want:
                self.err("file name must be <issue>-<slug>.md (lower-case slug)")
            elif int(want.group(1)) not in self.issues:
                self.err(f"file name issue {want.group(1)} is not in issue/issues")
        return self.errors

    def entry(self):
        refs = ", ".join(f"#{i}" for i in self.issues)
        lead = "**BREAKING** " if self.meta.get("breaking") else ""
        first, *rest = self.body["Summary"]
        out = [f"- {lead}**{self.meta['title'].rstrip('.')} ({refs}).** {first.strip()}"]
        out += ["  " + r.strip() if r.strip() else "" for r in rest]
        return out

    def upgrade(self):
        steps = self.body.get("Upgrade")
        if not steps:
            return []
        return [f"**#{self.issues[0]}:**"] + steps

    def sort_key(self):
        return (min(self.issues), self.name)


def trim(lines):
    while lines and not lines[0].strip():
        lines = lines[1:]
    while lines and not lines[-1].strip():
        lines = lines[:-1]
    return lines


def fragments():
    if not os.path.isdir(FRAG_DIR):
        return []
    names = sorted(n for n in os.listdir(FRAG_DIR)
                   if n.endswith(".md") and not n.startswith("_") and n != "README.md")
    return [Fragment(os.path.join(FRAG_DIR, n)) for n in names]


def split_changelog(text):
    """Return (head, unreleased body lines, tail) around the ## [Unreleased] section."""
    lines = text.splitlines()
    try:
        start = next(i for i, l in enumerate(lines) if l.strip() == UNRELEASED)
    except StopIteration:
        sys.exit(f"{CHANGELOG}: no '{UNRELEASED}' heading")
    end = next((i for i in range(start + 1, len(lines)) if lines[i].startswith("## ")), len(lines))
    return lines[:start], lines[start + 1:end], lines[end:]


def merge_sections(existing, frags):
    """Keep the existing Unreleased lines and add the fragment entries into the same ### sections."""
    intro, parts, order, cur = [], {}, [], None
    for line in existing:
        m = re.match(r"^### (.+?)\s*$", line)
        if m:
            cur = m.group(1)
            if cur not in parts:
                parts[cur] = []
                order.append(cur)
            continue
        (parts[cur] if cur else intro).append(line)
    parts = {k: trim(v) for k, v in parts.items()}
    for f in sorted(frags, key=Fragment.sort_key):
        parts.setdefault(f.meta["section"], []).extend(f.entry())
    ups = [f.upgrade() for f in sorted(frags, key=Fragment.sort_key) if f.upgrade()]
    if ups:
        u = parts.setdefault("Upgrade", [])
        for block in ups:
            if u:
                u.append("")
            u.extend(block)
    names = [s for s in SECTIONS if parts.get(s)]
    names += [s for s in order if s not in SECTIONS and s != "Upgrade" and parts.get(s)]
    if parts.get("Upgrade"):
        names.append("Upgrade")
    out = trim(intro)
    for s in names:
        if out:
            out.append("")
        out += [f"### {s}"] + parts[s]
    return out


def read_valid():
    frags = fragments()
    errs = [e for f in frags for e in f.validate()]
    if errs:
        print("\n".join(errs), file=sys.stderr)
        sys.exit(1)
    return frags


def cmd_check():
    frags = read_valid()
    print(f"ok: {len(frags)} fragment(s) in {os.path.relpath(FRAG_DIR, ROOT)}")


def cmd_preview():
    frags = [f for f in read_valid() if f.status == "done"]
    with open(CHANGELOG, encoding="utf-8") as fh:
        _, body, _ = split_changelog(fh.read())
    print(UNRELEASED)
    merged = merge_sections(body, frags)
    if merged:
        print()
        print("\n".join(merged))


def cmd_release(version, date):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?", version):
        sys.exit(f"release: '{version}' is not a SemVer version X.Y.Z[-pre]")
    if not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", date):
        sys.exit(f"release: '{date}' is not a date YYYY-MM-DD")
    frags = read_valid()
    done = [f for f in frags if f.status == "done"]
    with open(CHANGELOG, encoding="utf-8") as fh:
        text = fh.read()
    if re.search(rf"^## \[{re.escape(version)}\]", text, re.M):
        sys.exit(f"release: {CHANGELOG} already has a section for {version}")
    head, body, tail = split_changelog(text)
    merged = merge_sections(body, done)
    out = head + [UNRELEASED, "", f"## [{version}] - {date}"]
    if merged:
        out += [""] + merged
    out += [""] + tail if tail else []
    with open(CHANGELOG, "w", encoding="utf-8") as fh:
        fh.write("\n".join(out).rstrip("\n") + "\n")
    moved = 0
    for f in done:
        desc = f.body.get("Description")
        if desc:
            d = os.path.join(CHANGES_DIR, version)
            os.makedirs(d, exist_ok=True)
            refs = ", ".join(f"#{i}" for i in f.issues)
            meta = [f"Type: {f.meta['type']}. Section: {f.meta['section']}."]
            if f.meta.get("components"):
                meta.append(f"Components: {', '.join(f.meta['components'])}.")
            if f.meta.get("pr"):
                meta.append(f"PR: #{f.meta['pr']}.")
            page = [f"# {f.meta['title']} ({refs})", "", f"Released in {version} ({date}). " + " ".join(meta),
                    "", "## Summary", ""] + f.body["Summary"]
            if f.body.get("Upgrade"):
                page += ["", "## Upgrade", ""] + f.body["Upgrade"]
            page += ["", "## Description", ""] + desc
            with open(os.path.join(d, f.name), "w", encoding="utf-8") as fh:
                fh.write("\n".join(page) + "\n")
            moved += 1
    removed = [f for f in frags if f.status in ("done", "dropped")]
    for f in removed:
        os.remove(f.path)
    kept = len(frags) - len(removed)
    print(f"released {len(done)} fragment(s) into {version}; {moved} description(s) in "
          f"{os.path.relpath(os.path.join(CHANGES_DIR, version), ROOT)}; "
          f"{len(removed) - len(done)} dropped deleted; {kept} planned/in-progress kept")


def cmd_unreleased(path):
    text = sys.stdin.read() if path == "-" else open(path, encoding="utf-8").read()
    _, body, _ = split_changelog(text)
    print("\n".join(trim(body)))


def main(argv):
    cmd = argv[0] if argv else ""
    if cmd == "check" and len(argv) == 1:
        cmd_check()
    elif cmd == "preview" and len(argv) == 1:
        cmd_preview()
    elif cmd == "release" and len(argv) == 3:
        cmd_release(argv[1], argv[2])
    elif cmd == "unreleased" and len(argv) <= 2:
        cmd_unreleased(argv[1] if len(argv) == 2 else CHANGELOG)
    else:
        print(__doc__.split("\n\n")[1], file=sys.stderr)
        sys.exit(64)


if __name__ == "__main__":
    main(sys.argv[1:])
