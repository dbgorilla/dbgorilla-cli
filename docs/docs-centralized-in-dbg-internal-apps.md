# Docs live in dbg-internal-apps — not here

All internal documentation is centralized in one Docusaurus site:

- **Repo:** https://github.com/furiousengineering/dbg-internal-apps
- **Path:** `apps/docs/docs/`
- **Served:** internal portal at `/docs/`

Do not add docs to this repo. Only this pointer, `README.md`, and `CLAUDE.md` belong here.

## Writing or updating docs (humans and AI agents)

1. Work in a local clone of `dbg-internal-apps`. **AI agents:** if it is not
   cloned on this machine, stop and ask the user to clone it (or grant access) —
   do not fall back to writing docs in this repo.
2. Read `apps/docs/docs/writing-docs.mdx` and explore the existing categories
   (`cicd/`, `runbooks/`, `architecture/`, `dev-env/`, `repos/`) before writing.
   Extend an existing page before creating a new one; pick a topic category
   first, `repos/<this-repo>` only as a fallback.
3. Keep it succinct — signal over noise. If a doc is not worth maintaining,
   it is not worth writing.
