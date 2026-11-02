"""Build one valid data payload per conversation-item type, for the fixture.

The reference database has to describe rows the application can actually read.
Hand-writing ``{"messages": [...]}`` for every item type produced rows the Python
reader rejects: ``terminal_command`` requires a ``kind``, ``reasoning`` requires
an ``agent`` and a ``summary``, and so on. Rather than keep a second copy of that
schema here, this builds each payload through the production model classes, so a
field added to a payload later fails this generator instead of surfacing as a 500
in whichever server reads the row first.
"""

from __future__ import annotations


def build_item_payloads() -> dict[str, dict]:
    """Return ``{item_type: payload}`` for every type in the app registry.

    The payloads are deliberately minimal but complete: each satisfies its
    model's required fields so the reader reconstructs the typed object.
    """
    from omnigent.entities.conversation import (
        ITEM_TYPE_TO_DATA_CLS,
        CompactionData,
        ErrorData,
        FunctionCallData,
        FunctionCallOutputData,
        MessageData,
        NativeToolData,
        ReasoningData,
        ResourceEventData,
        RoutingDecisionData,
        SlashCommandData,
        TerminalCommandData,
    )

    payloads: dict[str, dict] = {
        "message": MessageData(
            role="assistant",
            agent="pi",
            content=[{"type": "output_text", "text": "Fixture message."}],
        ).model_dump(mode="json"),
        "function_call": FunctionCallData(
            agent="pi",
            name="read",
            arguments='{"path": "alpha_one.py"}',
            call_id="call-fixture-1",
        ).model_dump(mode="json"),
        "function_call_output": FunctionCallOutputData(
            call_id="call-fixture-1",
            output="contents of alpha_one.py",
        ).model_dump(mode="json"),
        "error": ErrorData(
            source="llm",
            code="fixture_error",
            message="Fixture error payload.",
        ).model_dump(mode="json"),
        "reasoning": ReasoningData(
            agent="pi",
            summary=[{"type": "summary_text", "text": "Fixture reasoning summary."}],
        ).model_dump(mode="json"),
        "compaction": CompactionData(
            summary="Fixture compaction summary.",
            last_item_id="item-fixture",
            token_count=1234,
        ).model_dump(mode="json"),
        "native_tool": NativeToolData(
            item={"id": "tool-fixture", "type": "web_search", "status": "completed"},
        ).model_dump(mode="json"),
        "resource_event": ResourceEventData(
            event_type="created",
            resource_id="resource-fixture",
            resource_type="terminal",
        ).model_dump(mode="json"),
        "routing_decision": RoutingDecisionData(
            model="deepseek-v4.1-flash",
            applied=True,
            rationale="Fixture routing decision.",
        ).model_dump(mode="json"),
        "slash_command": SlashCommandData(
            agent="pi",
            name="model",
            arguments="{}",
        ).model_dump(mode="json"),
        "terminal_command": TerminalCommandData(
            kind="input",
        ).model_dump(mode="json"),
    }

    # The stored column holds the model's *storage* form, not its wire form:
    # parse_item_data feeds it straight back into the model, which reads field
    # names ("agent"), while some models only rename on serialization ("model").
    # Writing the wire form makes every such row unreadable, so re-validate and
    # dump without aliases to keep the fixture loadable.
    payloads = {
        name: ITEM_TYPE_TO_DATA_CLS[name].model_validate(value).model_dump(mode="json")
        for name, value in payloads.items()
    }

    # A generator that silently stops covering a type would let a new item type
    # ship without fixture coverage, so check the key sets match exactly.
    expected = set(ITEM_TYPE_TO_DATA_CLS)
    missing = expected - set(payloads)
    extra = set(payloads) - expected
    if missing or extra:
        raise RuntimeError(
            "fixture payloads are out of sync with ITEM_TYPE_TO_DATA_CLS "
            f"(missing: {sorted(missing)}, unknown: {sorted(extra)})"
        )
    return payloads
