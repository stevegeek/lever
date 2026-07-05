# Assistant demo — workspace

This is a lever demo instance: a tiny personal assistant that runs a morning
standup (today's weather + your pending todos). It exists to show two things in
one runnable example — a **first-party** capability tool the broker supervises
(`todo`, reads `data/todos.csv`) and an **external** tool the broker only proxies
(`weather`) — plus grove dispatch and per-agent grants.

- Your todos live in `data/todos.csv` (id, task, due, priority, done).
- The `todo` grove reads them through the brokered `todo` tool.
- The manager holds the `weather` tool and orchestrates the standup.

<!-- lever:skills:begin -->
## Lever operator skill

Operating inside lever (brokered tools, capabilities, messaging, groves) is
documented in the `lever-operator` skill (`.claude/skills/lever-operator/`).
Consult it before using any brokered MCP tool.
<!-- lever:skills:end -->
