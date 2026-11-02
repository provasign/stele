const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const crypto = require('node:crypto');
function fixture() {
  const project = fs.mkdtempSync(path.join(os.tmpdir(), 'stele integration '));
  const specs = path.join(project, 'specs'), registry = path.join(project, 'registry');
  fs.mkdirSync(specs); fs.mkdirSync(registry);
  const file = path.join(specs, 'door.md');
  const original = '# Door\n\nREQ-1: The door MUST be locked.\n';
  const prose = 'Section: Door\nREQ-1: The door MUST be locked.';
  const hash = crypto.createHash('sha256').update(prose).digest('hex').slice(0, 16);
  const id = 'door.md#REQ-1';
  fs.writeFileSync(file, original);
  fs.writeFileSync(path.join(project, 'statements.yaml'), JSON.stringify([{ id, source: 'door.md#L3', prose, reference: [] }]));
  fs.writeFileSync(path.join(registry, 'context.yaml'), 'context: c\nformatVersion: 1\n---\nentity: Door\ncontext: c\nattributes:\n  locked: {type: bool}\n');
  fs.writeFileSync(path.join(registry, 'rules.yaml'), 'id: R1\nkind: invariant\nassert: door.locked\nsource: ' + JSON.stringify({ statementId: id, doc: 'door.md#L3', textHash: hash, statementHash: hash, prose }) + '\n');
  fs.writeFileSync(path.join(project, 'stele.yaml'), JSON.stringify({ formatVersion: 1, registry: 'registry', translate: { llm: 'claude', drafts: 1 }, specs: { root: 'specs', format: 'markdown', snapshot: 'statements.yaml' } }));
  return { project, specs, registry, file, original, dispose() { fs.rmSync(project, { recursive: true, force: true }); } };
}
module.exports = { fixture };
