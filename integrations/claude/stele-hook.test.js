const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { fixture } = require('../test/project');

test('Claude post-edit hook reports drift once and reports recovery, without a model', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const cache = path.join(f.project, 'cache'); fs.mkdirSync(cache);
  const invoke = () => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ hook_event_name: 'PostToolUse', cwd: f.project, session_id: 'integration-test' }),
      env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 5000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout) : null;
  };
  assert.equal(invoke(), null);
  fs.writeFileSync(f.file, f.original.replace('locked', 'unlocked'));
  const output = invoke();
  assert.equal(output.hookSpecificOutput.hookEventName, 'PostToolUse');
  // In the project's words: no absolute path, no diagnostic code.
  const note = output.hookSpecificOutput.additionalContext;
  assert.match(note, /the specs differ from what the owner approved \(requirements: 1 new or edited, 1 approved for text that has since changed\)/);
  assert.match(note, /- door\.md#REQ-1: edited since the owner approved it \(specs\/door\.md:3\)/);
  assert.match(note, /Next: run `stele change markdown \. --json`/);
  assert.ok(!note.includes(f.project) && !/spec\.changed|rule\.stale|provenance|stable rule IDs/.test(note), note);
  assert.equal(invoke(), null, 'unchanged drift spammed the context');
  fs.writeFileSync(f.file, f.original);
  assert.match(invoke().hookSpecificOutput.additionalContext, /match what the owner approved again/);
  assert.equal(invoke(), null);
});

test('Claude prompt hook injects the approved requirements a request touches, and nothing otherwise', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const ask = prompt => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ hook_event_name: 'UserPromptSubmit', cwd: f.project, session_id: 'integration-test', prompt }),
      env: { ...process.env, STELE_BIN: binary }, encoding: 'utf8', timeout: 5000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout) : null;
  };
  const output = ask('Change it so the door is not locked at night, for the cleaners');
  assert.equal(output.hookSpecificOutput.hookEventName, 'UserPromptSubmit');
  assert.match(output.hookSpecificOutput.additionalContext, /- REQ-1: The door MUST be locked\. \(door\.md#L3\)/);
  assert.match(output.hookSpecificOutput.additionalContext, /ask the owner/);
  assert.equal(ask('Bump all dependencies to their latest versions'), null);
});

test('Claude post-edit hook names the approved requirements a code edit touches, once', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const cache = path.join(f.project, 'cache'); fs.mkdirSync(cache);
  const git = (...args) => assert.equal(spawnSync('git', ['-C', f.project, '-c', 'user.name=t', '-c', 'user.email=t@t', '-c', 'commit.gpgsign=false', ...args]).status, 0);
  fs.writeFileSync(path.join(f.project, '.gitignore'), 'cache/\n');
  git('init', '-q', '-b', 'main'); git('add', '.'); git('commit', '-q', '-m', 'base');
  const invoke = () => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ hook_event_name: 'PostToolUse', cwd: f.project, session_id: 'touch-test' }),
      env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 10000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout) : null;
  };
  assert.equal(invoke(), null);
  fs.writeFileSync(path.join(f.project, 'door.go'), 'package door\n\ntype Door struct{ Locked bool }\n\nfunc Open(d *Door) { d.Locked = false }\n');
  const output = invoke();
  assert.match(output.hookSpecificOutput.additionalContext, /touches 1 approved requirement/);
  assert.match(output.hookSpecificOutput.additionalContext, /REQ-1: The door MUST be locked/);
  assert.match(output.hookSpecificOutput.additionalContext, /reads or writes locked|is locked/);
  assert.equal(invoke(), null, 'the same touched requirements spammed the context');
});

test('Claude post-edit hook reminds the agent of a request the owner has not approved, once', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const cache = path.join(f.project, 'cache'); fs.mkdirSync(cache);
  fs.writeFileSync(path.join(f.project, 'stele.yaml'), JSON.stringify({ formatVersion: 1, registry: 'registry', translate: { llm: 'claude', drafts: 1 }, specs: { root: '.', format: 'shale', snapshot: 'statements.yaml' } }));
  fs.writeFileSync(path.join(f.project, 'statements.yaml'), '[]');
  fs.writeFileSync(path.join(f.project, 'registry', 'rules.yaml'), '');
  fs.mkdirSync(path.join(f.project, '.shale'));
  fs.writeFileSync(path.join(f.project, '.shale', 's1.yaml'), 'id: s1\nintent:\n    title: Lock the door at night\n');
  // A request is a task unless kept as a requirement (stele keep): only
  // kept requests wait for the owner's approval.
  const keep = ids => {
    fs.mkdirSync(path.join(f.project, '.stele'), { recursive: true });
    fs.writeFileSync(path.join(f.project, '.stele', 'kept-intents.yaml'), ids.map(id => `- id: intent/${id}\n  title: t\n  by: agent\n  at: "2026-11-09"\n`).join(''));
  };
  const invoke = () => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ hook_event_name: 'PostToolUse', cwd: f.project, session_id: 'request-test' }),
      env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 10000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout) : null;
  };
  assert.equal(invoke(), null, 'a request not kept is a task: no reminder');
  keep(['s1']);
  const output = invoke();
  assert.match(output.hookSpecificOutput.additionalContext, /not approved by the owner yet: "Lock the door at night"/);
  assert.doesNotMatch(output.hookSpecificOutput.additionalContext, /run `run /, 'the next step was wrapped in a second run');
  const again = invoke();
  assert.ok(!again || !/not approved by the owner/.test(again.hookSpecificOutput.additionalContext), 'the reminder repeated');
  // Many waiting: the session's own by name, the rest as a count.
  for (let i = 2; i <= 6; i++) fs.writeFileSync(path.join(f.project, '.shale', `old${i}.yaml`), `id: old${i}\nintent:\n    title: Earlier request ${i}\n`);
  fs.writeFileSync(path.join(f.project, '.shale', 'request-test.yaml'), 'id: request-test\nintent:\n    title: Open the gate at dawn\n');
  keep(['s1', 'old2', 'old3', 'old4', 'old5', 'old6', 'request-test']);
  const many = invoke().hookSpecificOutput.additionalContext;
  assert.match(many, /not approved by the owner yet: "Open the gate at dawn" \(and 6 others\)/);
  assert.doesNotMatch(many, /Earlier request/);
});

test('Claude hook records the agent and the model in use for Stele\'s own calls', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const cache = path.join(f.project, 'cache'); fs.mkdirSync(cache);
  const invoke = input => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ cwd: f.project, session_id: 's1', ...input }),
      env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 5000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  const record = () => JSON.parse(fs.readFileSync(path.join(f.project, '.stele', 'cache', 'agent.json'), 'utf8'));
  assert.equal(invoke({ hook_event_name: 'SessionStart', source: 'startup', model: 'claude-opus-5-5' }), '', 'session start said something');
  assert.deepEqual(record(), { agent: 'claude', model: 'claude-opus-5-5', session: 's1' });
  invoke({ hook_event_name: 'PostToolUse', tool_name: 'Bash', tool_input: {} });
  assert.equal(record().model, 'claude-opus-5-5', 'an event without a model forgot it');
  assert.equal(invoke({ hook_event_name: 'PostModelSwitch', from_model: 'claude-opus-5-5', to_model: 'claude-sonnet-5-5' }), '');
  assert.equal(record().model, 'claude-sonnet-5-5');
  invoke({ hook_event_name: 'SessionStart', source: 'startup', session_id: 's2' });
  assert.deepEqual(record(), { agent: 'claude', model: '', session: 's2' }, 'a new session kept the old model');
});

test('Claude pre-tool hook asks the owner before an approval is recorded, and only then', t => {
  const f = fixture(); t.after(() => f.dispose());
  const binary = process.env.STELE_TEST_BIN || path.resolve(__dirname, '../../bin/stele-sync');
  const run = (command, extra = {}, env = {}) => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ hook_event_name: 'PreToolUse', tool_name: 'Bash', tool_input: { command }, cwd: f.project, session_id: 's', permission_mode: 'default', ...extra }),
      env: { ...process.env, STELE_BIN: binary, STELE_APPROVAL_PROMPT: '', ...env }, encoding: 'utf8', timeout: 5000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout).hookSpecificOutput : null;
  };
  // The summary of the last check is what the owner reads in the prompt.
  const work = path.join(f.project, '.stele', 'change', 'add-lock');
  fs.mkdirSync(work, { recursive: true });
  fs.writeFileSync(path.join(work, 'result.json'), JSON.stringify({ change: 'add-lock', added: ['x'], statements: { x: 'The door MUST be locked at night.' }, registry: f.registry }));
  // A spec tool names its changes, and the prompt says which one.
  const config = path.join(f.project, 'stele.yaml');
  fs.writeFileSync(config, fs.readFileSync(config, 'utf8').replace('"markdown"', '"openspec"'));
  const asked = run('stele change answer add-lock approve-1a2b approve');
  assert.equal(asked.hookEventName, 'PreToolUse');
  assert.equal(asked.permissionDecision, 'ask');
  assert.match(asked.permissionDecisionReason, /records your approval of "add-lock" as the owner/);
  assert.match(asked.permissionDecisionReason, /Stele read 1 requirement\./);
  // A change with no saved check still asks, without the summary.
  assert.equal(run('cd . && ./bin/stele change answer other q1 approve --by Ana').permissionDecision, 'ask');
  assert.equal(run('stele approve --registry registry --all --by Ana').permissionDecision, 'ask');
  // Every other command passes without a word.
  for (const command of ['stele change answer add-lock q1 show', 'stele change markdown . --json', 'stele approve --registry registry --list',
    'git commit -m "stele approve flow"', 'echo stele change answer a b approve', 'ls']) {
    assert.equal(run(command), null, command);
  }
  assert.equal(run('stele change answer add-lock q approve', { tool_name: 'Read' }), null);
  // Where Claude Code cannot ask, the agent asks in chat and says so.
  const denied = run('stele change answer add-lock q approve', { permission_mode: 'bypassPermissions' });
  assert.equal(denied.permissionDecision, 'deny');
  assert.match(denied.permissionDecisionReason, /ask them the approval question in chat.*--asked/s);
  assert.equal(run('stele change answer add-lock q approve --asked', { permission_mode: 'bypassPermissions' }), null);
  assert.equal(run('stele change answer add-lock q approve', { permission_mode: 'dontAsk' }).permissionDecision, 'deny');
  // A headless run holds the prompt with no one to answer: the owner's
  // yes in chat passes in any mode. The prompt carries no note to the agent.
  assert.doesNotMatch(asked.permissionDecisionReason, /Agent|--asked/);
  assert.equal(run('stele change answer add-lock q approve --asked'), null);
  // A requirements/ folder's change is named after its branch: the prompt leaves the name out.
  fs.writeFileSync(config, fs.readFileSync(config, 'utf8').replace('"openspec"', '"markdown"'));
  assert.match(run('stele change answer add-lock approve-1a2b approve').permissionDecisionReason, /records your approval as the owner.*Stele read 1 requirement\./);
  // An unattended run turns the prompt off where Claude Code starts.
  assert.equal(run('stele change answer add-lock q approve', {}, { STELE_APPROVAL_PROMPT: 'off' }), null);
});
