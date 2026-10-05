"""Extract the notification catalog from the flake-selected Vaultwarden source.

This updates classification only. The request exclusions are handwritten policy.
Run inside nix develop: python3 scripts/routes.py SOURCE --output OUTPUT
"""

import argparse
import re
from pathlib import Path


def body(source: str, function: str) -> str:
    match = re.search(r"\bfn\s+" + re.escape(function) + r"\s*\([^)]*\)[^{]*\{", source)
    if not match:
        raise ValueError(f"route function not found: {function}")
    start = match.end()
    depth = 1
    tokens = re.finditer(
        r'"(?:\\.|[^"\\])*"|//[^\n]*|/\*.*?\*/|[{}]', source[start:], re.DOTALL
    )
    for token in tokens:
        if token[0] == "{":
            depth += 1
        elif token[0] == "}":
            depth -= 1
            if depth == 0:
                return source[start : start + token.start()]
    raise ValueError("unclosed route function")


def collect(path: Path, function: str, prefix: str) -> set[str]:
    source = path.read_text()
    selected = body(source, function)
    result = set()
    declarations = {}
    for match in re.finditer(
        r'#\[(get|post|put|delete|patch|head|options)\("([^"\n]*)"[^\]]*\]'
        r"\s*(?:(?:#\[[^\]]*\]|//[^\n]*)\s*)*(?:pub\s+)?(?:async\s+)?fn\s+(\w+)",
        source,
    ):
        declarations[match[3]] = (match[1].upper(), match[2].split("?")[0])
    for group in re.findall(r"routes!\[([^\]]*)\]", selected, re.DOTALL):
        group = re.sub(r"//[^\n]*", "", group)
        for name in filter(None, map(str.strip, group.split(","))):
            method, route = declarations[name]
            if "..>" in route:
                continue  # Generic fallback handlers must not classify input.
            route = re.sub(r"<(\w+)>", r"{\1}", route)
            result.add(method + " " + prefix.rstrip("/") + route)
    directory = path.parent if path.name == "mod.rs" else path.with_suffix("")
    for module, child_function in re.findall(r"\b(\w+)::(\w*routes)\(\)", selected):
        child = directory / (module + ".rs")
        if not child.exists():
            child = directory / module / "mod.rs"
        result |= collect(child, child_function, prefix)
    return result


def catalog(source: Path) -> str:
    main = (source / "src/main.rs").read_text()
    # These exports are resolved from src/api/mod.rs and core/mod.rs. Fail on
    # changed mount names so a future audit reviews their mapping explicitly.
    exports = {
        "web_routes": ("web.rs", "routes"),
        "core_routes": ("core/mod.rs", "routes"),
        "admin_routes": ("admin.rs", "routes"),
        "core_events_routes": ("core/mod.rs", "events_routes"),
        "identity_routes": ("identity.rs", "routes"),
        "icons_routes": ("icons.rs", "routes"),
        "notifications_routes": ("notifications.rs", "routes"),
    }
    routes = set()
    mounts = re.findall(
        r'\.mount\(\[basepath, "([^"]*)"\]\.concat\(\), api::(\w+)\(\)\)', main
    )
    if not mounts:
        raise ValueError("no route mounts found")
    for prefix, exported in mounts:
        path, function = exports[exported]
        routes |= collect(source / "src/api" / path, function, prefix)
    if not routes:
        raise ValueError("empty route catalog")
    return "\n".join(sorted(routes)) + "\n"


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    args.output.write_text(catalog(args.source))
