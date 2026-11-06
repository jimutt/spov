# Local session history

A local `spov serve` keeps its sessions in a journal under
`$XDG_DATA_HOME/spov/sessions/` (`~/.local/share/spov/sessions/` by default),
so a stopped session is still listed, and can be resumed, after the server
exits. A hosted server keeps the same things in Postgres.

The layout follows Claude Code's convention of one append-only JSONL file per
session (`~/.claude/projects/<cwd>/<id>.jsonl`), rather than OpenCode's single
SQLite database, because spov's local engine is the in-memory store and only
needs its writes made durable (DL-091).

## Layout

```
sessions/
  .lock                      held by the running server; a second one refuses to start
  <session-id>/              mode 0700
    session.json             the session's row and its runs, rewritten whole
    transcript.jsonl         conversations, messages and events, appended
```

`session.json` is one line, `{"format":1,"session":{...},"runs":[...]}`. It is
replaced through a renamed temporary file, so it is always one version or the
other.

`transcript.jsonl` starts with a header and then holds one record per line:

```
{"type":"header","format":1,"session":"01K...","created":"2026-11-06T09:00:00Z"}
{"type":"conversation","conversation":{"id":"01K...","opencode_id":"ses_...","title":"...","info":{"json":{...}}, ...}}
{"type":"message","message":{"id":"01K...","conversation_id":"01K...","role":"assistant","text":"...","raw":{"json":{...}}, ...}}
{"type":"event","event":{"id":"01K...","pos":42,"seq":17,"source":"opencode","name":"session.step.started","raw":{"json":{...}}, ...}}
```

Each record is the stored row whole. A later record for the same conversation
or message ID replaces an earlier one, which is how a message updated at the
end of a turn is kept without editing the file. A byte field (`raw`, `info`) is
`{"json": ...}` when it holds compact JSON, and `{"base64": "..."}` otherwise.

## Reading it

```sh
cd ~/.local/share/spov/sessions/<session-id>
jq -c 'select(.type=="message") | {role: .message.role, text: .message.text}' transcript.jsonl
jq '.session | {name, state, stopped_at}' session.json
```

## Retention and repair

- A stopped session's directory is deleted `history.retention` (default
  `720h`, 30 days) after it stopped, when a server starts. `0` keeps
  everything. A session whose exit snapshot is still kept stays until the
  snapshot is pruned (DL-093).
- A last line cut short by a crash is dropped when the server next starts, and
  a transcript mostly made of superseded records is compacted then (DL-092).
- A directory whose `session.json` cannot be read, or whose `format` this
  build does not know, is skipped with a warning and left on disk.
- Deleting a session's directory while no server runs forgets it. Its
  sandbox, if one is still running, is adopted again by the reconciler.
