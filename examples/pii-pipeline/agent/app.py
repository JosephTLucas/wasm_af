"""
PII Redactor — a Bring Your Own Agent demo for WASM_AF.

Scans text for personally identifiable information using regex patterns,
redacts all matches, and returns a structured findings report.

This agent targets the `agent-untrusted` WIT world:
  - No host function imports (no LLM, no network, no shell, no email)
  - Only host-config for read-only configuration
  - Pure computation in a WASM sandbox

Build:
  componentize-py -d ../../../wit/agent.wit -w agent-untrusted componentize app -o pii_redactor.wasm
"""

import json
import re

from wit_world.imports.types import TaskInput, TaskOutput, KvPair

PII_PATTERNS = {
    "ssn": {
        "pattern": r"\b\d{3}-\d{2}-\d{4}\b",
        "label": "[REDACTED-SSN]",
        "description": "Social Security Number",
    },
    "email": {
        "pattern": r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b",
        "label": "[REDACTED-EMAIL]",
        "description": "Email Address",
    },
    "phone": {
        "pattern": r"\b(?:\+1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b",
        "label": "[REDACTED-PHONE]",
        "description": "Phone Number",
    },
    "credit_card": {
        "pattern": r"\b(?:\d{4}[-\s]?){3}\d{4}\b",
        "label": "[REDACTED-CC]",
        "description": "Credit Card Number",
    },
    "ip_address": {
        "pattern": r"\b(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\b",
        "label": "[REDACTED-IP]",
        "description": "IP Address",
    },
}


class WitWorld:
    """Implements the wasm-af:agent agent-untrusted world."""

    def execute(self, input: TaskInput) -> TaskOutput:
        text = _extract_text(input)
        if not text.strip():
            raise ValueError("no text found in context or payload")

        findings = {}
        redacted = text
        total_count = 0

        for pii_type, config in PII_PATTERNS.items():
            matches = re.findall(config["pattern"], redacted)
            if matches:
                count = len(matches)
                findings[pii_type] = {
                    "count": count,
                    "description": config["description"],
                    "samples": [m[:4] + "..." for m in matches[:3]],
                }
                redacted = re.sub(config["pattern"], config["label"], redacted)
                total_count += count

        result = {
            "redacted_text": redacted,
            "findings": findings,
            "summary": {
                "total_pii_found": total_count,
                "types_found": list(findings.keys()),
                "original_length": len(text),
                "redacted_length": len(redacted),
            },
        }

        return TaskOutput(
            payload=json.dumps(result, indent=2),
            metadata=[
                KvPair(key="pii_count", val=str(total_count)),
                KvPair(key="pii_types", val=",".join(findings.keys())),
            ],
        )


def _extract_text(input: TaskInput) -> str:
    """Pull document text from payload or ancestor context (url-fetch output)."""
    # Direct text in payload
    try:
        payload = json.loads(input.payload)
        if payload.get("text"):
            return payload["text"]
    except (json.JSONDecodeError, TypeError):
        pass

    # Ancestor step outputs in context (e.g. url-fetch → snippet)
    parts = []
    for kv in input.context:
        try:
            data = json.loads(kv.val)
            if isinstance(data, dict) and "results" in data:
                for r in data["results"]:
                    if isinstance(r, dict) and "snippet" in r:
                        parts.append(r["snippet"])
            elif isinstance(data, dict) and "text" in data:
                parts.append(data["text"])
            elif isinstance(data, dict) and "redacted_text" in data:
                parts.append(data["redacted_text"])
            else:
                parts.append(kv.val)
        except (json.JSONDecodeError, TypeError):
            parts.append(kv.val)

    return "\n".join(parts)
