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
  assert.match(output.hookSpecificOutput.additionalContext, /spec.changed/);
  assert.match(output.hookSpecificOutput.additionalContext, /1 stale rules/);
  assert.equal(invoke(), null, 'unchanged drift spammed the context');
  fs.writeFileSync(f.file, f.original);
  assert.match(invoke().hookSpecificOutput.additionalContext, /drift cleared/);
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
  const invoke = () => {
    const result = spawnSync(process.execPath, [path.join(__dirname, 'stele-hook.js')], {
      input: JSON.stringify({ hook_event_name: 'PostToolUse', cwd: f.project, session_id: 'request-test' }),
      env: { ...process.env, STELE_BIN: binary, STELE_HOOK_CACHE: cache }, encoding: 'utf8', timeout: 10000
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim() ? JSON.parse(result.stdout) : null;
  };
  const output = invoke();
  assert.match(output.hookSpecificOutput.additionalContext, /not approved by the owner yet: "Lock the door at night"/);
  const again = invoke();
  assert.ok(!again || !/not approved by the owner/.test(again.hookSpecificOutput.additionalContext), 'the reminder repeated');
});
