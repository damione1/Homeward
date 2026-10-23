#!/usr/bin/env python3
"""Record installed package ownership and retain the actual distributed notices.

Runs inside the pinned build images. No downloads, credentials or live services.
"""
import argparse
import hashlib
import importlib.metadata
import json
from pathlib import Path
import shutil
import subprocess
import sys
import sysconfig


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def packages():
    fields = "${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\t${Architecture}\n"
    output = subprocess.check_output(["dpkg-query", "-W", "-f", fields], text=True)
    result = {}
    for line in output.splitlines():
        name, version, source, source_version, arch = line.split("\t")
        result[name] = dict(name=name, version=version, source=source or name.split(":")[0],
                            source_version=source_version or version, architecture=arch)
    return result


def retain_docs(name, output):
    directory = Path("/usr/share/doc") / name.split(":")[0]
    docs = {}
    if directory.is_dir():
        # Keep the entire package documentation, including compressed copyright,
        # upstream NOTICE and third-party attribution files, resolving symlinks.
        for path in sorted(directory.rglob("*")):
            if path.is_file():
                target = output / "licenses" / "debian" / name / path.relative_to(directory)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(path, target)
                docs[str(target.relative_to(output))] = digest(target)
    return docs


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("runtime",))
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    installed = packages()
    result = {"schema_version": 1, "debian": {}, "debian_files": {}, "python": []}
    selected = set(installed)
    for name in sorted(selected):
        result["debian"][name] = {**installed[name], "documents": retain_docs(name, args.output)}
        paths = subprocess.check_output(["dpkg-query", "-L", name], text=True).splitlines()
        for filename in paths:
            path = Path(filename)
            if path.is_absolute() and path.is_file():
                item = {"owner": name, "sha256": digest(path)}
                result["debian_files"].setdefault(str(path), item)
                result["debian_files"].setdefault(str(path.resolve()), item)
    if args.mode == "runtime":
        shutil.copytree("/usr/share/common-licenses", args.output / "licenses" / "debian-common", dirs_exist_ok=True)
        license_file = args.output / "licenses" / "python-interpreter" / "LICENSE.txt"
        license_file.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile("/usr/local/lib/python3.13/LICENSE.txt", license_file)
        interpreter_files = {}
        for path in (Path(sys.executable), Path(sysconfig.get_config_var("LIBDIR")) / sysconfig.get_config_var("LDLIBRARY")):
            interpreter_files[str(path)] = digest(path)
            interpreter_files[str(path.resolve())] = digest(path)
        result["python_interpreter"] = {"version": sys.version.split()[0], "files": interpreter_files,
                                       "documents": {str(license_file.relative_to(args.output)): digest(license_file)}}
        for distribution in sorted(importlib.metadata.distributions(), key=lambda d: d.metadata["Name"].lower()):
            name = distribution.metadata["Name"]
            item = {"name": name, "version": distribution.version,
                    "license_expression": distribution.metadata.get("License-Expression", ""),
                    "license": distribution.metadata.get("License", ""),
                    "project_urls": distribution.metadata.get_all("Project-URL") or [],
                    "home_page": distribution.metadata.get("Home-page", ""),
                    "documents": {}, "native_files": {}, "files": {}}
            for relative in sorted(distribution.files or [], key=str):
                path = Path(distribution.locate_file(relative))
                if not path.is_file():
                    continue
                item["files"][str(path.resolve())] = digest(path)
                if ".so" in path.name or path.suffix in (".a", ".dll", ".dylib"):
                    item["native_files"][str(path)] = digest(path)
                if any(word in str(relative).lower() for word in ("license", "licence", "notice", "copyright", "copying", "authors")):
                    # Preserve directory structure; wheel notices can describe
                    # bundled OpenSSL, Rust crates or other native components.
                    safe = [p for p in relative.parts if p not in ("..", ".", "/")]
                    target = args.output / "licenses" / "python" / name / Path(*safe)
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(path, target)
                    item["documents"][str(target.relative_to(args.output))] = digest(target)
            result["python"].append(item)
    (args.output / (args.mode + "-inventory.json")).write_text(json.dumps(result, sort_keys=True, indent=2) + "\n")


if __name__ == "__main__":
    main()
