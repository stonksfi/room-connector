# Room plugin for Claude Code

Shows this Claude Code session in the Room app as a little character that is
working, waiting for you or sleeping, streams its conversation there, and lets
you send it a message from your phone.

It needs `roomd`, the Room connector, running on the same machine:

```sh
roomd install   # installs the daemon and this plugin
roomd login     # prints a code; enter it in the Room app
```

What it does:

- Hooks (`hooks/hooks.json`) forward each session event (start, prompt, tool
  use, a permission prompt or question waiting on you, the assistant's reply,
  stop, end) to `roomd` over its local socket in `~/.room`. If `roomd` is not
  installed they do nothing.
- An in-session script (`hooks/register.ts`) polls `roomd` for messages sent
  from the app and submits them as your prompt, so an idle session wakes up and
  a busy one runs the message right after its current turn.
- While Claude asks you a question or shows you a plan, the script waits beside
  the dialog for an answer from the app: tap an option there (or keep planning
  with feedback) and it answers the dialog. Whichever answer comes first, here
  or in the app, wins. Approving a plan stays at your Mac.
- When in-session scripts are turned off, the Stop hook hands a waiting message
  to Claude at the end of a turn instead.

What leaves your machine: your prompts, Claude's replies, the questions it asks
you and the plans it proposes, one-line summaries of tool calls, and each
session's folder, name and state. Messages and answers are only accepted for
sessions on this machine, from the Room account the machine was linked to.
