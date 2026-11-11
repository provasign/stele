# Try Stele

This is the preview guide: an hour with Stele on a small project of your own, and what to send back. You need no knowledge of solvers or rule languages. You talk to your coding agent as usual, and Stele works through it.

## What you need

- **git**, and a repository you can experiment in. A new one is best for a first try.
- **[Claude Code](https://claude.com/claude-code),** signed in: the one coding agent Stele supports for now. Stele reads your specs through it, so its spend lands on that account. `stele init` names Sonnet 5.5 for Stele's own calls (`translate.model` in `stele.yaml`); on it a short requirement costs about 2 cents to read, so a feature of 5 to 15 requirements costs about $0.10 to $0.30 of Stele's calls, on top of what the agent itself spends. Long requirements with scenarios cost several times that, and a larger model about four times.
- **[Z3](https://github.com/Z3Prover/z3)**, the solver: `brew install z3`, or your system's package (`apt install z3`).
- **[Node.js](https://nodejs.org)**, which runs Stele's agent hooks.
- **Optional:** [Spec Kit](https://github.com/github/spec-kit), [OpenSpec](https://github.com/Fission-AI/OpenSpec) or [Kiro](https://kiro.dev), if you already write specs with one. Without one, `stele init` sets up a `requirements/` folder of plain Markdown that the agent keeps; requests declared with [Shale](https://github.com/provasign/shale) work too.

## Install

```sh
brew install provasign/tap/stele
# or: curl -fsSL https://raw.githubusercontent.com/provasign/stele/main/install.sh | bash
```

Then, in your repository:

```sh
stele init
git add -A && git commit -m "Set up Stele"
```

`init` finds your spec format, or sets up `requirements/` when there is none, writes Stele's short instructions and hooks for Claude Code, and says what is missing. A new repository needs no first commit before the first check.

## The first hour

Build something small, one feature at a time, and ask your agent for each feature the way you would ask a colleague. The trial project was a community garden tool:

1. "Keep a list of our garden plots. Each plot has a number and a size. I need commands to add a plot and list them. Plot numbers are unique."
2. "Members join with a name and an email address. No two members may share an email."
3. "A member can book a free plot. A plot has at most one holder, and a member holds at most one plot."
4. "A member can release a plot, and it becomes free."
5. "A waitlist: when a plot is released, it goes straight to the first member waiting."

Choose your own domain; the point is several features that build on each other, so that later ones can contradict earlier ones.

Say which rules last: "from now on", "never", "at most". In a project with no spec tool, the agent writes those into `requirements/` and builds the rest as an ordinary task.

For each feature, the agent writes the spec, runs Stele's check, and comes back to you with:

- **Questions only you can answer,** in the spec's own words. For example, at the waitlist feature:

  > The new waitlist rule clashes with an earlier one. Booking spec, FR-003: a member holds at most one plot. Waitlist spec, FR-006: a released plot goes to the first member waiting. If the first member waiting already holds a plot, they would hold two. Can a member on the waitlist hold a plot?

  You answer in plain words. The agent updates the spec, and Stele remembers what you said (the lessons in `.stele/registry/lessons/`).
- **Findings,** when there are any: where Stele's reading says more than your text, or would block a step. Before the approval the agent also names each clause of a requirement that no rule checks, so that you know what only tests will guard, and can show every requirement clause by clause with what checks it in plain words.
- **The approval.** The agent runs the command that approves, and Claude Code asks you to allow it, with Stele's summary in the prompt. Allow it to approve; deny it and nothing is recorded. Approving pins Stele's reading of each requirement. From then on, every later change is checked against it. In a session that never asks for permissions, or a headless run with no one at the prompt, the agent asks you in chat instead.

Then the agent writes the code and commits the specs, the registry and the code together.

## If the project already has specs

In a project with OpenSpec, Spec Kit or Kiro specs, say: "This project already has specs. Set them up with Stele." The agent runs `stele adopt`, which reads every requirement once, checks them against each other, and comes back with what needs you.

- **Start with one or two specs on a large project.** On more than a hundred requirements the agent first shows you the specs with their sizes and asks which to start with; nothing is read or spent before you choose. `stele adopt --list` shows each spec and how many requirements it holds; `stele adopt --spec <name>` reads only the ones you name, and a later run reads more, checked against what you approved. On a project of ours, two specs with 76 requirements took about five minutes and $1.90 for the first check and asked the owner nothing in its first round; all 179 took about ten minutes and $4, and asked five questions.
- **You are asked only about contradictions a reader of your specs would see.** Stele's rules clash in more places than that, mostly where a spec leaves a gap any reader fills in. Those it settles itself and counts for you in one line; the report lists them.
- **You do not have to settle every contradiction to finish.** Specs written over years disagree in places. For each contradiction the agent offers the answers that change nothing in your specs: one requirement is the exception to the other (recommended when one is a check or an error case), or decide later, which records the contradiction as open and lets you approve the rest. `stele check` lists what is open.
- **Some contradictions are possible, not proved.** Stele also reads your specs' texts for contradictions. One its rules cannot prove is listed before the approval as possible, in one sentence. It needs no answer: reword a requirement if it is real, or approve knowing of it.
- **Not everything can be checked.** Expect a quarter to a third of the requirements to be recorded unchecked: those about how the code is built, about the process, or matters of judgment. The summary gives the count and the report lists each.
- **Stele sets aside a rule of its own when it misread a requirement** and that rule clashes with another requirement. The summary says how many, and the report names each, so you can see what is then not checked.

## What to look for

- Did Stele ask about something real that you would have missed?
- Did it ask about something that was not a problem? What did the question say?
- Was any question hard to understand, or in Stele's language rather than yours?
- Did the agent ever get stuck: the same question back after you answered it, or a check that would not finish?
- How long did each check take, and what did it cost?

## Known limits

- **Comparisons that ignore upper and lower case are not checked.** "Emails are the same ignoring case" is reported as a clause Stele does not check. Cover it with tests.
- **Steps that build rich text can go unchecked.** A listing whose lines combine several values per item can be beyond the solver. Stele says so, and the owner approves those rules knowing they are not checked.
- **A rerun can read a requirement differently.** That is why you approve each reading. If Stele misreads one, tell the agent what the requirement means; it records that, and Stele reads it again.
- **Stele checks the specs, not the code.** After an edit, the agent hears which approved requirements the code touches, but nothing proves that the code keeps them.

## What to expect from the readings

Stele's reading of a requirement is a model's, and a second run can read the same sentence differently: a contradiction found in one run can go unfound in the next, and some questions come with no recommended answer. What Stele reports as a contradiction is checked by a solver and is one among its rules; what it does not report is not proof there is none. Before you approve, the agent shows each requirement clause by clause, with what checks it or that nothing does: that list, not the count, is what you approve.

## What to send back

Open an issue at [provasign/stele](https://github.com/provasign/stele/issues), or reply to whoever sent you this guide, with:

- `stele version` and `claude --version`;
- for each question you thought was wrong or unclear: the question as the agent put it, and what you answered;
- anything that got stuck, with `.stele/change/<feature>/report.md` attached if you can share it (it is there until you approve the change; a reading that failed also leaves its model calls in `failed-calls/` beside it);
- your answers to the questions under "What to look for".

Leave out anything private: the registry and reports quote your specs.
