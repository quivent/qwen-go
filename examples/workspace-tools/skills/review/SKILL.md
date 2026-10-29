---
name: evidence-review
description: Inspect a change and report evidence-backed correctness findings
---
Read the workspace instructions and the changed code. Trace each suspected defect through its callers and tests. Use read-only subagents for independent checks when that helps. Confirm findings in the actual files before reporting. Distinguish a reproducible bug from a missing assumption. Do not edit or execute shell commands without approval. Include concise file/line references and a concrete trigger for each finding.
