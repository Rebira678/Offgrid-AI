# Day 23: The AI never overwrites a human

**Done when:** a human edit made offline survives an AI job finishing later. It does, and a test names it: `TestAHumanEditMadeOfflineSurvivesAnAIJobFinishingLater` (two replicas) and `TestAHumanEditSurvivesAnAIJobThatFinishesAfterIt` (a real queue and worker). `make ai-suggests-demo` shows it with two real processes.

## The rule, and how it is enforced

> A model may **create** a task. It may never **change** one. Anything it wants to change about an existing task is a *suggestion*, stored beside the task, that only a person can apply.

"Be careful" is not a rule you can test. A structure is. So the rule is not in the model's prompt or in each tool; it is in what the model-side code is **handed**:

- `internal/suggest` wraps the task store given to any model-driven code. Reading and creating pass straight through. `Update` and `AddNote` do not write: they record a suggestion and return a `*Suggested` error, so no caller can mistake it for success.
- The tools (`update_task`, `add_note`) do not know the wrapper is there. A tool added next month cannot forget the rule, because the way to write an existing task is simply not reachable from the model's side.
- `Resolve` (accept or dismiss) is on a separate interface, `task.Suggester`, that only the command line holds. A test fake panics if the AI side ever calls it.

## What is stored

Two new kinds of op, so suggestions sync like everything else:

| Op | Meaning | Changes a task? |
|---|---|---|
| `suggestion.made` | "model M proposes: task T, status/priority/note", plus **what T looked like then** (`base_status`, `base_priority`) | **No.** The task row, its notes and its updated time are untouched. |
| `suggestion.resolved` | a person accepted or dismissed suggestion (node, seq) | Only if accepted, and then as an ordinary edit. |

A suggestion's ID is `node.seq` of the op that made it (`laptop.12`): the same on every device, with no coordination.

## Stale: why accept can be refused

A suggestion remembers the task as it was when proposed. If a person has changed a field the suggestion would change, the suggestion is **stale**: it was written against a task that no longer looks like that. `accept` on a stale suggestion is refused and changes nothing; `dismiss` always works. A suggested note is never stale (it adds, it does not replace). A field the suggestion does not touch does not make it stale.

Accepting is a person editing the task, so it also counts for delete's add-wins (Day 20): if one person deletes a task while another accepts a suggestion on it, the task survives.

## Two devices, one suggestion

If two devices resolve the same suggestion differently (one accepts, one dismisses), the **earlier** resolution in the canonical fold order wins on both, and a later one is ignored. The same fold order that makes task fields converge makes this converge, and a random test (12 seeds, three replicas, humans and AIs interleaved) checks that every replica ends with the same hash and that a suggestion never changes a task by itself.

## What changed in the commands

| Command | What it does now |
|---|---|
| `offgrid drain` | A queued `interpret` job creates new tasks as before, but an update or note on an existing task becomes a suggestion. Retrying a job proposes once (the job's key). A suggestion needs no crash marker; creating a task keeps the Day 12 one. |
| `offgrid suggestions -db F` | list pending (`-all` for resolved, `-json`), `accept ID`, `dismiss ID` |
| `offgrid exec -as-ai -by NAME -db F` | run a command the way a model's commands are run. It is how the demo plays the AI, and how you can try the rule by hand. |

Live Tier 1 (`offgrid agent`, you typing and watching the answer) is unchanged: that is you, in front of the screen, asking for the change.

## Honest limits

- **Stale is judged on the device where you accept,** from the edits it has received. An edit on another device that has not synced yet is invisible; once it syncs, the suggestion shows STALE. In the demo that is exactly what happens.
- **The base is the task at the moment the suggestion is made,** not when the message was typed. A person who edits between typing and the AI finishing sees the suggestion as pending, not stale; accepting it is their explicit choice, with the base shown in `offgrid suggestions`.
- The AI stand-in in the demo is a typed command. No language model is involved. The 7-8B and cloud paths are still untested against real models.
- Suggestions are never deleted or compacted; the log only grows.
- `deep_think` answers (Day 22) are text for a person to read; they do not propose changes. Turning an answer into structured suggestions is a later step.

## What is tested

- Store: a suggestion changes nothing in the task and adds exactly one op; keys make proposing idempotent; accept applies status, priority and note once; dismiss changes nothing; stale accept is refused and changes nothing, both for a person's edit made before and after; device agreement on every path; out-of-order arrival and rebuild give the same hash; malformed remote ops are refused and leave nothing behind; delete vs accept.
- Wrapper: only fields that would change are proposed; no tool command can change an existing task (through the real registry).
- Handler and worker: update and note become suggestions; creating still works with its crash marker; retries propose once; refusals and no-ops propose nothing; a person's edit survives a job finishing later.
- CLI: `exec -as-ai`, `suggestions` (list, accept, dismiss, stale, errors, JSON), and a full `drain` run.
- Mutation testing: about 22 deliberate bugs in the new code; every one is caught. (Three survivors found and closed with new tests, including "accepting does not count as an edit for delete".) One mutation run was killed by a tool timeout and left a bug in my working copy; I caught it, restored the file and reran the mutants.

## Try it

```bash
make ai-suggests-demo
offgrid exec -seed -db tasks.db '{"intent":"list_tasks"}'
offgrid exec -as-ai -by qwen -db tasks.db '{"intent":"update_task","task_ref":"pump","priority":"low"}'
offgrid suggestions -db tasks.db
offgrid suggestions -db tasks.db accept laptop.4     # your ID will differ
```

## The post (Day 23)

> The AI never overwrites a human.
>
> My agent can create tasks. It cannot change one. When a model wants to update a task, the change is stored beside it as a suggestion, with a note of what the task looked like at that moment.
>
> If a person edited the task since, accepting the suggestion is refused. Dismissing always works. The task is never touched by the model, so an AI job finishing hours later can't undo an edit someone made offline.
>
> It is enforced by what the model-side code is handed, not by asking it to be careful: the store it gets has no way to write an existing task.
>
> Suggestions sync as ops like everything else. Two devices that resolve one differently agree on the earlier resolution.
>
> Demo: two processes, a person and a (scripted) AI disagreeing offline.

Hook for AI engineers: *do not ask the model to be careful; hand it a store that cannot write.* Attach `docs/day-23-human-wins.png`.

## Checklist

- [ ] `go get pgregory.net/rapid@v1.2.0`, commit `go.mod` and `go.sum` (still needed from Day 17).
- [ ] `make ai-suggests-demo` (about 10 seconds, no model needed).
- [ ] With real Ollama: queue a message that updates a task while offline (`offgrid agent -route -net offline`), edit that task by hand, then `offgrid drain -once`; check `offgrid suggestions`.
- [ ] 15 minutes of outreach.
