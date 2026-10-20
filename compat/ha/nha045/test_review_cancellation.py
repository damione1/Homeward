"""Exercise only the exact pure cancellation gate; never import HA on host."""

import ast
from dataclasses import dataclass, replace
from pathlib import Path

import pytest


def cancellation_gate():
    filename = Path(__file__).with_name("ha_worker.py")
    parsed = ast.parse(filename.read_text(), filename=str(filename))
    names = {"CancellationRequest", "NativeCancellation", "ExpectedCancellation", "admit_cancelled_task"}
    selected = [node for node in parsed.body if isinstance(node, (ast.ClassDef, ast.FunctionDef)) and node.name in names]
    assert {node.name for node in selected} == names
    namespace = {"dataclass": dataclass, "__name__": __name__}
    # Compile just these four stdlib-only definitions. All HA imports, Worker
    # construction, asyncio entry points and effects remain outside this AST.
    exec(compile(ast.Module(body=selected, type_ignores=[]), str(filename), "exec"), namespace)
    return namespace


def test_nha_045_cancelled_task_requires_native_stop_terminal_and_direct_join():
    gate = cancellation_gate()
    Request, Native, Evidence = (gate[name] for name in ("CancellationRequest", "NativeCancellation", "ExpectedCancellation"))
    admit = gate["admit_cancelled_task"]
    request = Request("a", "ha-a", 0, True)
    parent = Native("a", "ha-a", 2, True, True, True)
    child = Native("a/child/1", "ha-child", 1, True, True, True)
    valid = Evidence(request, True, parent, (child,), True)
    admit(valid)
    for invalid in (
        None,
        replace(valid, task_cancelled=False),
        replace(valid, request=replace(request, stop_joined=False)),
        replace(valid, request=replace(request, stop_joined=1)),
        replace(valid, request=replace(request, raw="another-run")),
        replace(valid, parent=replace(parent, cancelled=False)),
        replace(valid, parent=replace(parent, stopped=False)),
        replace(valid, parent=replace(parent, joined=False)),
        replace(valid, parent=replace(parent, terminal_position=0)),
        replace(valid, children=()),
        replace(valid, children=(replace(child, terminal_position=3),)),
        replace(valid, children=(replace(child, cancelled=False),)),
        replace(valid, children=(replace(child, stopped=False),)),
        replace(valid, children=(replace(child, joined=False),)),
        replace(valid, children=(replace(child, raw=parent.raw),)),
    ):
        with pytest.raises(RuntimeError):
            admit(invalid)


def test_nha_045_detached_child_is_not_admitted_as_a_direct_join():
    gate = cancellation_gate()
    Request, Native, Evidence = (gate[name] for name in ("CancellationRequest", "NativeCancellation", "ExpectedCancellation"))
    request = Request("a", "ha-a", 0, True)
    parent = Native("a", "ha-a", 1, True, True, True)
    gate["admit_cancelled_task"](Evidence(request, True, parent, (), False))
    child = Native("a/child/1", "ha-child", 2, True, True, True)
    with pytest.raises(RuntimeError):
        gate["admit_cancelled_task"](Evidence(request, True, parent, (child,), False))
