const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { fixture } = require('../test/project');

// Codex runs each hook command in a shell, with no project-directory
// variable, from wherever the session is. Its events carry Claude Code's
// fields and more (model, turn_id, permission_mode, transcript_path).
test('Codex hook commands find the script from a subdirectory and answer in Codex\'s format', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  assert.equal(spawnSync('git', ['init', '-q'], { cwd: f.project }).status, 0);
  const script = path.join(f.project, '.codex', 'hooks', 'stele-hook.js');
  fs.mkdirSync(path.dirname(script), { recursive: true });
  fs.copyFileSync(path.join(__dirname, '..', 'claude', 'stele-hook.js'), script);
  const sub = path.join(f.project, 'src', 'deep'); fs.mkdirSync(sub, { recursive: true });
  const hooks = JSON.parse(fs.readFileSync(path.join(__dirname, 'hooks.example.json'), 'utf8')).hooks;
  const cache = path.join(f.project, 'cache'); fs.mkdirSync(cache);
  const run = (event, extra) => {
    const result = spawnSync('sh', ['-c', hooks[event][0].hooks[0].command], {
      cwd: sub,
      input: JSON.stringify({ hook_event_name: event, cwd: sub, session_id: 's', model: 'm', turn_id: 't', permission_mode: 'default', transcript_path: null, ...extra }),
      env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 5000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout) : null;
  };
  const prompt = run('UserPromptSubmit', { prompt: 'Change it so the door is not locked at night, for the cleaners' });
  assert.equal(prompt.hookSpecificOutput.hookEventName, 'UserPromptSubmit');
  assert.match(prompt.hookSpecificOutput.additionalContext, /The door MUST be locked/);
  assert.equal(run('UserPromptSubmit', { prompt: 'Bump all dependencies to their latest versions' }), null);
  const tool = { tool_name: 'apply_patch', tool_input: {}, tool_response: {}, tool_use_id: 'u' };
  assert.equal(run('PostToolUse', tool), null);
  fs.writeFileSync(f.file, f.original.replace('locked', 'unlocked'));
  const drift = run('PostToolUse', tool);
  assert.equal(drift.hookSpecificOutput.hookEventName, 'PostToolUse');
  assert.match(drift.hookSpecificOutput.additionalContext, /1 approved for text that has since changed/);
});

test('Codex hook records Codex and the model of every event', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const script = path.join(f.project, '.codex', 'hooks', 'stele-hook.js');
  fs.mkdirSync(path.dirname(script), { recursive: true });
  fs.copyFileSync(path.join(__dirname, '..', 'claude', 'stele-hook.js'), script);
  const cache = path.join(f.project, 'cache'); fs.mkdirSync(cache);
  const result = spawnSync(process.execPath, [script], {
    input: JSON.stringify({ hook_event_name: 'UserPromptSubmit', cwd: f.project, session_id: 's', model: 'gpt-5.5', turn_id: 't', prompt: 'hello' }),
    env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 5000
  });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(f.project, '.stele', 'cache', 'agent.json'), 'utf8')),
    { agent: 'codex', model: 'gpt-5.5', session: 's' });
});
