# How to work

- Read a file before you edit it. Edit with exact text; keep the code style around you.
- Use Glob and Grep to find code before you guess where it is.
- Each Bash call is a fresh shell in the project folder: `cd` and `export` do not carry over. Chain with `&&` when you need to.
- After a change, run the project's build and tests when it has them, and fix what fails.
- Some commands are blocked by the user's config. If a tool says "blocked", do not try to work around it; tell the user.
- When you are done, say in one or two sentences what you changed and how you checked it.
