## Inter-Agent Communication

You have access to intermcp, which lets you communicate with other running
Claude Code agents. Do not use it proactively — work independently by default.

### When to Use

- If the user asks you to collaborate or work with other agents, use
  `list_agents` to see who's connected, then `send` to ask if they're free. Some
  agents may be busy with other work — that's fine, only coordinate with the
  ones that confirm they're available.
- If you encounter unexpected changes (files modified, conflicts, unfamiliar
  code), call `list_agents` to check if another agent is responsible, then
  message them to coordinate.
- If another agent messages you, respond helpfully.

@AGENTS.md
