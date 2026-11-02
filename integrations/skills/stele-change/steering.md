## Stele: keep the specs consistent

This project's specs are checked by Stele (`stele.yaml` at the repository root names the approved registry). Whenever you write or edit specs (OpenSpec `openspec/changes/`, Spec Kit `specs/`, Kiro `.kiro/specs/`), and when a `stele gate` run fails:

1. Run `stele change openspec . <change-id> --json` (OpenSpec), or `stele change speckit|kiro . --base <branch> --json` (Spec Kit, Kiro). Read the JSON: `status`, `questions`, `report`.
2. Questions with `ask_owner: false` are yours: Stele misread a requirement. Record the fix with `stele change answer <id> <question-id> <choice> --by agent` (or `stele change reread <id> <requirement> --note "..." --by agent`).
3. Put questions with `ask_owner: true` to the owner, one at a time, in the spec's words, never Stele's rule syntax or symbol names. Offer the `recommended` option first with its `why`. Record each answer with `stele change answer <id> <question-id> <choice>` (add `--note` with the owner's words where an option needs them).
4. Run the check again after answering; repeat until the approval question comes. Before asking it, tell the owner about every `requirements[].findings` entry (a clause Stele's reading does not check, or a reading broader than the text).
5. The owner's answers teach Stele about this project: the approval question names the lessons they add (`lessons/` in the registry). Say in one line what Stele will remember, and remind the owner to commit the registry, `lessons/` included, with the change. Your own answers (`--by agent`) teach nothing.

When `stele.yaml` says `format: shale`, the project's requests are its specs: each `shale intent` you declare is a requirement. Right after declaring one, before the first edit, run `stele change shale . --base <branch> --json` and follow the steps above. To change a request, declare it again with `shale intent`, showing the owner the new wording. `update_old` and `exception` answers retire the earlier request themselves; after `exception`, declare the new request again as the whole rule with its exception.

To adopt a project's existing specs (the registry has no rules yet, or a Stele message says so), run `stele adopt --json` and follow it exactly as `stele change` below, with `adopt` as the change id; it asks the owner only what needs a person, then for the approval.

When Stele lists approved requirements a request touches, plan within them. If the plan must change one, name it and ask the owner before planning around it. When a Stele message blocks a commit or says `Next:`, do what it says; never bypass Stele with `--no-verify`.

Never edit the registry by hand, never approve on the owner's behalf, and never answer an `ask_owner: true` question yourself.
