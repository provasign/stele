# Try Stele

This is the preview guide: an hour with Stele on a small project of your own, and what to send back. You need no knowledge of solvers or rule languages. You talk to your coding agent as usual, and Stele works through it.

## What you need

- **git**, and a repository you can experiment in. A new one is best for a first try.
- **[Claude Code](https://claude.com/claude-code),** signed in: the one coding agent Stele supports for now. Stele reads your specs through it, with the model it is using, so its spend lands on that account. Measured on a 60-requirement set with Claude Code: about 2 cents per requirement on Sonnet 5.5 and 8 cents on Opus 5.5, so a feature of 5 to 15 requirements costs about $0.10 to $1.20 of Stele's own calls, on top of what the agent itself spends; a feature that needs many rereads costs more. `translate.model` in `stele.yaml` pins a model for Stele's calls alone.
- **[Z3](https://github.com/Z3Prover/z3)**, the solver: `brew install z3`, or your system's package (`apt install z3`).
- **[Node.js](https://nodejs.org)**, which runs Stele's agent hooks.
- **Optional:** [Spec Kit](https://github.com/github/spec-kit), [OpenSpec](https://github.com/Fission-AI/OpenSpec) or [Kiro](https://kiro.dev), if you already write specs with one. Without one, plain Markdown requirements work, and so do requests declared with [Shale](https://github.com/provasign/shale).

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

`init` finds your spec format, writes Stele's short instructions and hooks for Claude Code, and says what is missing.

## The first hour

Build something small, one feature at a time, and ask your agent for each feature the way you would ask a colleague. The trial project was a community garden tool:

1. "Keep a list of our garden plots. Each plot has a number and a size. I need commands to add a plot and list them. Plot numbers are unique."
2. "Members join with a name and an email address. No two members may share an email."
3. "A member can book a free plot. A plot has at most one holder, and a member holds at most one plot."
4. "A member can release a plot, and it becomes free."
5. "A waitlist: when a plot is released, it goes straight to the first member waiting."

Choose your own domain; the point is several features that build on each other, so that later ones can contradict earlier ones.

For each feature, the agent writes the spec, runs Stele's check, and comes back to you with:

- **Questions only you can answer,** in the spec's own words. For example, at the waitlist feature:

  > The new waitlist rule clashes with an earlier one. Booking spec, FR-003: a member holds at most one plot. Waitlist spec, FR-006: a released plot goes to the first member waiting. If the first member waiting already holds a plot, they would hold two. Can a member on the waitlist hold a plot?

  You answer in plain words. The agent updates the spec, and Stele remembers what you said (the lessons in `.stele/registry/lessons/`).
- **Findings:** clauses Stele's reading does not check, so that you know what only tests will guard.
- **The approval question.** Approving pins Stele's reading of each requirement. From then on, every later change is checked against it.

Then the agent writes the code and commits the specs, the registry and the code together.

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

## What to send back

Open an issue at [provasign/stele](https://github.com/provasign/stele/issues), or reply to whoever sent you this guide, with:

- `stele version` and `claude --version`;
- for each question you thought was wrong or unclear: the question as the agent put it, and what you answered;
- anything that got stuck, with `.stele/change/<feature>/report.md` attached if you can share it;
- your answers to the questions under "What to look for".

Leave out anything private: the registry and reports quote your specs.
