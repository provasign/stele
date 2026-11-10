#!/usr/bin/env node
// Claude Code and Codex adapter (their hook events share one format).
// UserPromptSubmit: the approved requirements the request touches, for the
// plan to keep. PreToolUse (Bash): a command that records the owner's
// approval asks the owner, in Claude Code's own permission prompt.
// PostToolUse: saved-spec drift, and the approved requirements the code
// change touches.
// No shell interpolation, model invocation or rule writes.
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const crypto = require('node:crypto');
const { spawnSync } = require('node:child_process');

let event = 'PostToolUse';

// The agent this hook runs in and the model it is using, recorded for
// Stele's own calls (translate.llm: agent) under the project. Codex names
// the model in every event; Claude Code at session start (not always) and
// on a model switch, so the model stands until the same session names
// another. The agent is known from where `stele init` put the script.
function recordAgent(project, input) {
  const agent = __dirname.split(path.sep).includes('.codex') ? 'codex' : 'claude';
  const file = path.join(project, '.stele', 'cache', 'agent.json');
  let previous = {};
  try { previous = JSON.parse(fs.readFileSync(file, 'utf8')); } catch {}
  const session = input.session_id || '';
  const kept = previous.agent === agent && previous.session === session ? previous.model : '';
  const record = { agent, model: input.to_model || input.model || kept || '', session };
  if (['agent', 'model', 'session'].every(k => previous[k] === record[k])) return;
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, JSON.stringify(record) + '\n');
  } catch {} // a read-only checkout: Stele falls back to the CLI it finds
}

// A command that records an approval: `stele change answer <change>
// <question> approve`, or `stele approve` (not its --list). The owner
// approves, never the agent, and an instruction alone did not hold an eager
// agent back. This catches the command as Stele's instructions give it; it
// is a guard against a mistake, not a security boundary.
function approvalCommand(command) {
  // Where a command starts, so a commit message that names one is not one.
  const start = String.raw`(?:^|[;&|(\n])\s*(?:\w+=\S*\s+)*(?:\S*/)?stele\s+`, end = String.raw`(?=$|[\s;&|)])`;
  const answer = new RegExp(start + String.raw`change\s+answer\s+(\S+)\s+\S+\s+approve` + end).exec(command);
  if (answer) return { change: answer[1].replace(/^["']|["']$/g, '') };
  const direct = new RegExp(start + String.raw`approve` + end + String.raw`([^;&|]*)`).exec(command);
  if (direct && !/(^|\s)--?list\b/.test(direct[1])) return { change: '' };
  return null;
}

// PreToolUse: ask the owner before an approval is recorded. Where Claude
// Code cannot ask (permissions bypassed or never asked, or a headless run
// that holds every prompt), the agent asks in chat and says so with
// --asked. A headless run held the command, the agent asked in chat, and
// the same command with --asked was held again (a trial, 2026-11-09): the
// flag passes in every mode. STELE_APPROVAL_PROMPT=off, set where Claude
// Code starts, leaves approvals to unattended runs.
function approval(project, input) {
  if (input.tool_name !== 'Bash' || process.env.STELE_APPROVAL_PROMPT === 'off') return;
  const command = String(input.tool_input?.command || '');
  const found = approvalCommand(command);
  if (!found) return;
  const decide = (permissionDecision, permissionDecisionReason) => process.stdout.write(JSON.stringify({
    hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision, permissionDecisionReason }
  }) + '\n');
  if (/(^|\s)--asked\b/.test(command)) return;
  if (['bypassPermissions', 'dontAsk'].includes(input.permission_mode)) {
    decide('deny', 'Stele: the owner approves a change, and Claude Code is not asking for permissions in this session, so the owner has not seen this approval. ' +
      'Show the owner the spec and the check\'s summary and ask them the approval question in chat. When they say yes, run the same command again with --asked added. Never add it before they answer.');
    return;
  }
  let what = '';
  if (found.change) {
    const summary = spawnSync(process.env.STELE_BIN || 'stele', ['change', 'summary', found.change],
      { cwd: project, encoding: 'utf8', timeout: 4000, maxBuffer: 1024 * 1024 });
    if (!summary.error && summary.status === 0) what = summary.stdout.trim();
  }
  decide('ask', `Stele: this records your approval${found.change ? ` of "${found.change}"` : ''} as the owner of this project's requirements.` +
    (what ? ' ' + what : '') + ' Allow it to approve; deny it to hold the change back.' +
    ' (Agent: if this was held with no one to answer, ask the owner the approval question in chat and add --asked only after their yes.)');
}

function main() {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  event = input.hook_event_name || event;
  let project = path.resolve(input.cwd || process.cwd());
  while (!fs.existsSync(path.join(project, 'stele.yaml'))) {
    const parent = path.dirname(project);
    if (parent === project) return;
    project = parent;
  }
  if (event === 'PreToolUse') return approval(project, input);
  recordAgent(project, input);
  if (event === 'SessionStart' || event === 'PostModelSwitch') return;
  if (input.hook_event_name === 'UserPromptSubmit') {
    // Prevention, never blocking: say nothing when the request touches no
    // approved requirement, or when Stele cannot answer.
    const rules = spawnSync(process.env.STELE_BIN || 'stele', ['rules', '--project', project],
      { cwd: project, input: input.prompt || '', encoding: 'utf8', timeout: 5000, maxBuffer: 1024 * 1024 });
    if (!rules.error && rules.status === 0 && rules.stdout.trim()) {
      process.stdout.write(JSON.stringify({
        hookSpecificOutput: { hookEventName: 'UserPromptSubmit', additionalContext: rules.stdout.trim() }
      }) + '\n');
    }
    return;
  }
  const notes = [];
  const key = crypto.createHash('sha256').update(project + '\0' + (input.session_id || '')).digest('hex');
  // Say a thing once: each part is repeated only when it changes.
  const changed = (part, text) => {
    const cache = path.join(process.env.STELE_HOOK_CACHE || os.tmpdir(), `stele-hook-${key}${part}`);
    const digest = crypto.createHash('sha256').update(text).digest('hex');
    let previous = '';
    try { previous = fs.readFileSync(cache, 'utf8'); } catch {}
    fs.writeFileSync(cache, digest, { mode: 0o600 });
    return { now: previous !== digest, before: previous };
  };
  // A project whose stele.yaml names no specs has none to keep in sync:
  // say nothing, rather than a failure on every command (2026-11-07).
  let config = '';
  try { config = fs.readFileSync(path.join(project, 'stele.yaml'), 'utf8'); } catch {}
  // `specs:` at the start of a line, or `"specs":` in the flow (JSON)
  // form YAML also reads; CI's fixtures write the latter (2026-11-09).
  const hasSpecs = /(^|[{,]\s*)["']?specs["']?\s*:/m.test(config);
  const result = hasSpecs ? spawnSync(process.env.STELE_BIN || 'stele',
    ['watch', '--project', project, '--once', '--json'],
    { cwd: project, encoding: 'utf8', timeout: 10000, maxBuffer: 8 * 1024 * 1024 }) : null;
  if (!result) {
    // no specs configured
  } else if (result.error || result.status !== 0) {
    const why = String(result.error?.message || result.stderr || result.stdout).trim();
    if (changed('-error', why).now) notes.push(`Stele could not compare the specs with what the owner approved: ${why}. Do not assume they match.`);
  } else {
    const status = JSON.parse(result.stdout);
    // A request declared with shale intent that the owner has not approved:
    // the code written for it waits on that approval. Said once per set of
    // waiting requests; an agent once built a feature and asked after.
    const waiting = (status.pending || []).map(s => ({ id: String(s.ID ?? s.id), prose: String(s.Prose ?? s.prose ?? '') })).filter(s => s.id.startsWith('intent/'));
    if (waiting.length > 0 && changed('-requests', waiting.map(s => s.id).join('\n')).now) {
      // This session's requests by name (a request's ID carries the session
      // it was declared in), the others as a count: 25 waiting requests
      // were all listed each time one was added (2026-11-06).
      const session = String(input.session_id || '');
      let named = session ? waiting.filter(s => s.id.includes(session)) : [];
      if (named.length === 0) named = waiting.slice(-3);
      named = named.slice(-3);
      const others = waiting.length - named.length;
      const command = /`([^`]+)`/.exec(status.next || '');
      notes.push(`Stele: ${named.length === 1 ? 'this request is' : 'these requests are'} not approved by the owner yet: ` +
        named.map(s => `"${s.prose.split('\n')[0].slice(0, 120)}"`).join('; ') +
        (others > 0 ? ` (and ${others} other${others === 1 ? '' : 's'})` : '') +
        `. Before more edits for ${named.length === 1 && others === 0 ? 'it' : 'them'}, run ${command ? '`' + command[1] + '`' : 'Stele\'s change check'} and put its questions, and the approval, to the owner.`);
    }
    const seen = changed('', result.stdout);
    if (seen.now && status.in_sync) {
      if (seen.before) notes.push('Stele: the specs match what the owner approved again.');
    } else if (seen.now) {
      // In the project's words and paths: the note named absolute paths,
      // diagnostic codes and "stable rule IDs" (a review, 2026-11-09).
      const plain = {
        'spec.added': 'a new requirement, not checked yet',
        'spec.changed': 'edited since the owner approved it',
        'spec.moved': 'moved; nothing to decide',
        'spec.provenance': 'moved in its file: run `stele sync --apply` to record where it is now; nothing to decide',
        'spec.removed': 'approved, and no longer in the specs',
        'rule.stale': 'approved for text that has since changed',
        'rule.orphaned': 'approved, and its requirement is gone',
      };
      const where = d => { const rel = path.relative(project, path.resolve(project, String(d.file || ''))); return rel.startsWith('..') ? path.basename(rel) : rel; };
      const count = (n, what) => n > 0 ? [`${n} ${what}`] : [];
      const counts = [...count((status.pending || []).length, 'new or edited'), ...count((status.stale_rules || []).length, 'approved for text that has since changed'),
        ...count((status.orphan_rules || []).length, 'approved and no longer in the specs')];
      // One line a requirement: its edit says what the rule's line would.
      const edited = new Set((status.diagnostics || []).filter(d => d.code === 'spec.changed').map(d => d.statement));
      const diagnostics = (status.diagnostics || []).filter(d => !(d.code === 'rule.stale' && edited.has(d.statement)));
      const lines = [...new Set(diagnostics.map(d => `- ${d.statement || where(d)}: ${plain[d.code] || d.message}` +
        (String(d.code).startsWith('spec.') && d.code !== 'spec.removed' ? ` (${where(d)}:${d.line})` : '')))];
      notes.push(`Stele: the specs differ from what the owner approved${counts.length ? ' (requirements: ' + counts.join(', ') + ')' : ''}.\n` +
        lines.slice(0, 8).join('\n') +
        (lines.length > 8 ? `\n(and ${lines.length - 8} more)` : '') +
        (status.next ? `\nNext: ${status.next}. Nothing is checked or approved until then.` : ''));
    }
  }
  // The approved requirements the code change touches, when that set changes.
  const touches = spawnSync(process.env.STELE_BIN || 'stele', ['touches', '--project', project],
    { cwd: project, encoding: 'utf8', timeout: 8000, maxBuffer: 1024 * 1024 });
  if (!touches.error && touches.status === 0) {
    const text = touches.stdout.trim();
    const ids = text.split('\n').filter(l => l.startsWith('- ')).join('\n');
    if (changed('-touches', ids).now && text) notes.push(text);
  }
  if (notes.length) {
    process.stdout.write(JSON.stringify({
      hookSpecificOutput: { hookEventName: 'PostToolUse', additionalContext: notes.join('\n\n') }
    }) + '\n');
  }
}
try { main(); } catch (error) {
  if (event === 'UserPromptSubmit') return; // prevention stays silent when it cannot answer
  if (event === 'PreToolUse') return; // a failed guard never blocks a command
  process.stdout.write(JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PostToolUse', additionalContext: `Stele's hook failed (${error.message}), so it is not known whether the specs match what the owner approved.`
  } }) + '\n');
}
