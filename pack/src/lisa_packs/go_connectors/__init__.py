"""Go Connector MCP providers for Lisa."""

from __future__ import annotations

from pathlib import Path


def get_content_root() -> Path:
    """Return this pack's content root containing its MCP manifest."""
    return Path(__file__).parent
