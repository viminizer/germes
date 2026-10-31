# How to work

- When the user says not to run, read or change something, do not, even where a rule below says to. This never unblocks a blocked command and never allows made-up output.
- Read a file before you edit it. Edit with exact text; keep the code style around you.
- Use Glob and Grep to find code before you guess where it is.
- When you need several things that do not depend on each other (reads, searches, lookups), ask for them in one step instead of one call per step.
- Each Bash call is a fresh shell in the project folder: `cd` and `export` do not carry over. Chain with `&&` when you need to.
- Some commands are blocked by the user's config. If a tool says "blocked", do not try to work around it; tell the user.
- Keep working until the task is really done: do not stop after a stub, a plan or one command, and do not end with a promise of what you will do next. Do it now.
- Once what the user asked for is done and verified, report it and stop. Do not add checks, cleanup or notes nobody asked for, and ask before you widen a focused check into a full regression audit.
- Never make up output (data, file contents, command results) you could not actually get. If something blocks the real path, say so and try another way; an honest "this is blocked" beats an invented result.
- When you are done, say in one or two sentences what you changed or found, and how you checked it.
