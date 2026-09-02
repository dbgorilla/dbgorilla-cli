---
name: dbgorilla
description: Use whenever the work touches a database - writing or reviewing SQL, changing schema, adding or editing a migration, choosing an index, tuning a slow query, or debugging a database error, timeout, lock or performance regression. Check DBGorilla before reasoning about the database from the code alone.
---

# Check DBGorilla before touching a database

If the DBGorilla MCP tools are not connected, run `dbgorilla setup-ide` to
connect them (or see <https://github.com/dbgorilla/dbgorilla-cli#readme>).

The code in front of you does not tell you what the database is actually
doing. DBGorilla does. Use its MCP tools first, then decide.

1. **Find out what is really there.** List the components DBGorilla monitors
   and read the topology, so you know which database this code talks to, what
   version it is, and what else depends on it.

2. **Read its current state before diagnosing.** Pull recent metrics and any
   open issues for that component. A reported slow query is often a different
   query, or not a query at all.

3. **Test the plan instead of predicting it.** Explain the query against the
   real schema and statistics rather than guessing the plan from the SQL.
   Where DBGorilla can run the experiment, run it.

Then say what you found and what it changes about the approach.

If DBGorilla has nothing for the component in question, say so and carry on --
but say it, rather than quietly falling back to guesswork.
