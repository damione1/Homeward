"""Pure native-hook tests. The selected AST never imports or constructs HA."""

import ast
import asyncio
from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace

import pytest


def native_hooks():
    filename = Path(__file__).with_name("ha_worker.py")
    parsed = ast.parse(filename.read_text(), filename=str(filename))
    worker = next(node for node in parsed.body if isinstance(node, ast.ClassDef) and node.name == "Worker")
    observer = next(node for node in worker.body if isinstance(node, ast.FunctionDef) and node.name == "install_observers")
    trace = next(node for node in observer.body if isinstance(node, ast.ClassDef) and node.name == "Trace")
    selected = [node for node in parsed.body if isinstance(node, ast.ClassDef) and node.name in {"NativePoint", "PendingAction"}]
    point = next(node for node in worker.body if isinstance(node, ast.FunctionDef) and node.name == "native_point")
    point.name = "emit_native_point"

    class OriginalTrace:
        def __init__(self, _hass, _native, _stop, _variables):
            pass

        async def __aenter__(self):
            return "actual-parent-entry-result"

        async def __aexit__(self, *_args):
            return False

    class SyntheticError(Exception):
        pass

    execution = SimpleNamespace(raw="ha-a", scope="node/a/root/1")
    state = SimpleNamespace(native_points=[], native_ordinal=0, raw_nodes=[], pending_actions=[], elapsed_ms=0)
    namespace = {"dataclass": dataclass, "__name__": __name__, "original_trace": OriginalTrace,
                 "worker": state, "CURRENT": SimpleNamespace(get=lambda: execution),
                 "script": SimpleNamespace(trace_path_get=lambda: "sequence/0/parallel/1/sequence/0"),
                 "HomeAssistantError": SyntheticError}
    # Only stdlib dataclasses, the actual point method and actual Trace hook.
    # No Worker.__init__, imports, task runner or Docker code is compiled here.
    exec(compile(ast.fix_missing_locations(ast.Module(body=[*selected, point, trace], type_ignores=[])),
                 str(filename), "exec"), namespace)
    state.native_point = lambda key: namespace["emit_native_point"](state, key)
    return namespace, state


def test_nha_045_native_callback_counter_is_bounded_unique_and_observational():
    gate, state = native_hooks()
    state.native_point("a/start")
    state.native_point("state/switch.nha045_gate/2")
    assert [(point.key, point.ordinal) for point in state.native_points] == [
        ("a/start", 1), ("state/switch.nha045_gate/2", 2)]
    for invalid in (None, 0, "", "x" * 257, "a/start"):
        with pytest.raises(RuntimeError):
            state.native_point(invalid)
    assert state.native_ordinal == 2
    state.native_points = [gate["NativePoint"](str(index), index + 1) for index in range(1024)]
    state.native_ordinal = 1024
    with pytest.raises(RuntimeError):
        state.native_point("overflow")
    assert state.native_ordinal == len(state.native_points) == 1024


def test_nha_045_action_issue_is_witnessed_at_exact_native_exit():
    gate, state = native_hooks()
    Trace, Pending = gate["Trace"], gate["PendingAction"]

    async def observe():
        trace = Trace(None, SimpleNamespace(_action={}), None, None)
        assert await trace.__aenter__() == "actual-parent-entry-result"
        action = Pending("a/action/switch.nha045_other/1", "ha-a", "node/a/root/1", trace.path, False)
        state.pending_actions.append(action)
        # An input/effect point already observed at the real port remains
        # before its later action-result hook; no collection-time reorder.
        state.native_point("state/switch.nha045_other/2")
        assert not any(point.key == action.key for point in state.native_points)
        assert await trace.__aexit__(None, None, None) is False
        assert state.pending_actions == []
        assert [(point.key, point.ordinal) for point in state.native_points] == [
            ("state/switch.nha045_other/2", 1), (action.key, 2)]

    asyncio.run(observe())


@pytest.mark.parametrize("mismatch", ["run", "scope", "path", "duplicate", "outcome"])
def test_nha_045_native_action_hook_refuses_ambiguous_or_foreign_issue(mismatch):
    gate, state = native_hooks()

    async def observe():
        trace = gate["Trace"](None, SimpleNamespace(_action={}), None, None)
        await trace.__aenter__()
        action = gate["PendingAction"]("a/action/switch.nha045_other/1",
                                      "foreign" if mismatch == "run" else "ha-a",
                                      "node/a/root/0" if mismatch == "scope" else "node/a/root/1",
                                      "sequence/foreign" if mismatch == "path" else trace.path,
                                      mismatch == "outcome")
        state.pending_actions.append(action)
        if mismatch == "duplicate":
            state.pending_actions.append(action)
        if mismatch in ("duplicate", "outcome"):
            with pytest.raises(RuntimeError):
                await trace.__aexit__(None, None, None)
        else:
            assert await trace.__aexit__(None, None, None) is False
        assert state.pending_actions  # collect must reject any unresolved issue
        assert state.native_points == []

    asyncio.run(observe())
