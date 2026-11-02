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

function main() {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  event = input.hook_event_name || event;
  let project = path.resolve(input.cwd || process.cwd());
  while (!fs.existsSync(path.join(project, 'stele.yaml'))) {
    const parent = path.dirname(project);
    if (parent === project) return;
    project = parent;
  }
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
  const result = spawnSync(process.env.STELE_BIN || 'stele',
    ['watch', '--project', project, '--once', '--json'],
    { cwd: project, encoding: 'utf8', timeout: 10000, maxBuffer: 8 * 1024 * 1024 });
  if (result.error || result.status !== 0) {
    notes.push(`Stele could not check saved specs: ${result.error?.message || result.stderr || result.stdout}. Do not treat the specs as synchronized.`);
  } else {
    const status = JSON.parse(result.stdout);
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
