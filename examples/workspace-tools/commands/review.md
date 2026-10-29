---
description: Review current changes with independent read-only subagents
---
Review the current workspace changes. Read AGENTS.md first. Spawn independent read-only subagents for correctness, tests, and security, giving each a concrete scope. Spawn them before waiting so the work runs concurrently. Inspect their reports and verify actionable findings against the actual files. Report bugs with file/line references, explain their impact, and omit speculative concerns. Do not edit files. Use the native Git MCP plugin if useful; connecting and calling it requires user approval.

Additional focus: $ARGUMENTS
