#!/usr/bin/env node
// Claude Code and Codex adapter (their hook events share one format).
// UserPromptSubmit: the approved requirements the request touches, for the
// plan to keep. PostToolUse: saved-spec drift, and the approved
// requirements the code change touches.
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

function main() {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  event = input.hook_event_name || event;
  let project = path.resolve(input.cwd || process.cwd());
  while (!fs.existsSync(path.join(project, 'stele.yaml'))) {
    const parent = path.dirname(project);
    if (parent === project) return;
    project = parent;
  }
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
  const hasSpecs = /^specs:/m.test(config);
  const result = hasSpecs ? spawnSync(process.env.STELE_BIN || 'stele',
    ['watch', '--project', project, '--once', '--json'],
    { cwd: project, encoding: 'utf8', timeout: 10000, maxBuffer: 8 * 1024 * 1024 }) : null;
  if (!result) {
    // no specs configured
  } else if (result.error || result.status !== 0) {
    const why = String(result.error?.message || result.stderr || result.stdout).trim();
    if (changed('-error', why).now) notes.push(`Stele could not check saved specs: ${why}. Do not treat the specs as synchronized.`);
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
      if (seen.before) notes.push('Stele: saved-spec drift cleared; tracked rule provenance matches again.');
    } else if (seen.now) {
      const diagnostics = status.diagnostics || [];
      notes.push(`Stele found saved-spec changes requiring attention (${(status.pending || []).length} statements pending, ${(status.stale_rules || []).length} stale rules, ${(status.orphan_rules || []).length} orphaned rules).\n` +
        diagnostics.slice(0, 8).map(d => `${d.file}:${d.line}: ${d.code}: ${d.message}`).join('\n') +
        (diagnostics.length > 8 ? `\n${diagnostics.length - 8} more diagnostics; run stele watch --once --json for the complete plan.` : '') +
        (status.next ? `\nNext: ${status.next}.` : '') +
        '\nRefresh only affected requirements, preserve stable rule IDs, and review removals. This hook has not translated or approved changes.');
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
  process.stdout.write(JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PostToolUse', additionalContext: `Stele sync hook failed: ${error.message}. Sync status is unknown.`
  } }) + '\n');
}
