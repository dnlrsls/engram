import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { PLUGIN_ROOT, importPluginFromSandbox, withPluginSandbox } from './plugin-sandbox.mjs';

const expected = JSON.parse(await readFile(join(PLUGIN_ROOT, 'runtime-contract.json'), 'utf8'));

async function withRuntimeFixture(contract, body, options = {}) {
  await withPluginSandbox('engram-runtime-contract-', async ({ dir, sandbox }) => {
    const saved = Object.fromEntries(Object.entries(process.env).filter(([key]) => key.startsWith('ENGRAM_') || key.startsWith('GENTLE_')));
    for (const key of Object.keys(saved)) delete process.env[key];
    const requests = [], effects = [], statuses = [], warnings = [];
    const state = { identity: '1'.repeat(32), contract }; 
    const server = createServer(async (req, res) => {
      const path = new URL(req.url, 'http://fixture').pathname;
      let raw = ''; for await (const chunk of req) raw += chunk;
      requests.push({ method: req.method, path });
      if (req.method !== 'GET' && req.method !== 'HEAD') effects.push({ method: req.method, path, body: raw });
      res.setHeader('content-type', 'application/json');
      if (path === '/runtime-contract') {
        res.statusCode = options.status ?? 200;
        res.end(options.raw ?? JSON.stringify({ instance_id: state.identity, ...state.contract }));
      } else if (path === '/health') {
        res.end(JSON.stringify({ status: 'ok', service: 'engram', version: options.version ?? '3.0.0', instance_id: state.identity, capabilities: { root_session_resume: true, isolated_session_registration: true, runtime_session_resolution: true } }));
      } else if (path === '/project/current') {
        res.end(JSON.stringify({ project: 'runtime-contract-tests' }));
      } else if (path === '/sessions') {
        res.end(JSON.stringify({ id: JSON.parse(raw).id, status: 'created' }));
      } else if (path === '/stats') {
        res.end(JSON.stringify({ total_observations: effects.filter(x => x.path === '/observations').length }));
      } else {
        res.end(JSON.stringify({ context: '', id: 1, status: 'saved' }));
      }
    });
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    process.env.ENGRAM_URL = `http://127.0.0.1:${server.address().port}`;
    // Explicit URLs must not probe/start a local binary to establish ownership.
    process.env.ENGRAM_BIN = join(dir, 'must-not-be-invoked');
    const tools = new Map(), hooks = new Map();
    const ctx = { cwd: dir, hasUI: true, sessionManager: { getSessionId: () => 'runtime-contract-session', getBranch: () => [] }, ui: { setStatus: (_key, value) => statuses.push(value), notify: message => warnings.push(message) } };
    try {
      const register = await importPluginFromSandbox(sandbox);
      register({ registerTool: tool => tools.set(tool.name, tool), on: (event, handler) => hooks.set(event, handler), appendEntry() {} });
      await body({ ctx, tools, hooks, requests, effects, statuses, warnings, state });
    } finally {
      server.closeAllConnections();
      await new Promise(resolve => server.close(resolve));
      for (const key of Object.keys(process.env)) if (key.startsWith('ENGRAM_') || key.startsWith('GENTLE_')) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
}

async function assertRejected(contract, options = {}) {
  await withRuntimeFixture(contract, async ({ ctx, tools, hooks, requests, effects, statuses }) => {
    await hooks.get('session_start')({}, ctx);
    await hooks.get('before_agent_start')({ prompt: 'A sufficiently long human prompt for rejected capture.', systemPrompt: '' }, ctx);
    const result = await tools.get('mem_save').execute('rejected-save', { title: 'Must not persist', content: 'Rejected contract must leave memory unchanged.' }, undefined, undefined, ctx);
    await hooks.get('session_compact')({ summary: 'Rejected compaction must not archive.' });
    await hooks.get('session_shutdown')({ reason: 'quit' }, ctx);
    assert.equal(tools.size, 22, 'registration remains unchanged even when qualification fails');
    assert.equal(result.isError, true);
    assert.match(result.content[0].text, /runtime contract/i);
    assert.match(result.content[0].text, /upgrade|update|restart/i);
    assert.deepEqual(effects, [], 'rejected qualification leaves all session/memory counters unchanged');
    assert.equal(requests.some(r => !['/health', '/runtime-contract'].includes(r.path)), false, 'only read-only qualification metadata may be requested');
    assert.equal(statuses.some(value => typeof value === 'string' && /\bready\b/i.test(value)), false);
  }, options);
}

for (const [name, contract, options] of [
  ['missing endpoint on a version-matching legacy server', expected, { status: 404 }],
  ['malformed JSON', expected, { raw: '{' }],
  ['null response', expected, { raw: 'null' }],
  ['unknown contract', { ...expected, contract_id: 'unknown' }],
  ['legacy Foundation contract', { ...expected, status: 'mcp-foundation' }],
  ['unknown revision', { ...expected, revision: 2 }],
  ['missing identity', { ...expected, instance_id: undefined }],
  ['malformed identity', { ...expected, instance_id: '   ' }],
  ['unexpected field', { ...expected, unexpected: true }],
  ['mutated operation with unchanged claimed digest', { ...expected, operations: Object.fromEntries(Object.entries(expected.operations).slice(1)) }],
]) {
  test(`runtime qualification rejects ${name} before every lifecycle/save effect`, () => assertRejected(contract, options));
}

test('matching remote contract preserves all tools and dispatches without local binary ownership or SemVer fallback', async () => {
  await withRuntimeFixture(expected, async ({ ctx, tools, hooks, requests, effects, statuses }) => {
    await hooks.get('session_start')({}, ctx);
    const prompt = 'A matching contract permits this human prompt to be captured.';
    await hooks.get('before_agent_start')({ prompt, systemPrompt: '' }, ctx);
    const result = await tools.get('mem_stats').execute('matching-stats', {}, undefined, undefined, ctx);
    assert.equal(tools.size, 22);
    assert.notEqual(result.isError, true);
    assert.equal(requests.some(r => r.path === '/stats'), true);
    assert.equal(effects.some(r => r.path === '/sessions'), true);
    const captured = effects.find(r => r.path === '/prompts');
    assert.ok(captured, 'the positive lifecycle must dispatch prompt capture');
    assert.deepEqual(JSON.parse(captured.body), { session_id: 'runtime-contract-session', content: prompt, project: 'runtime-contract-tests' });
    assert.equal(statuses.some(value => typeof value === 'string' && /\bready\b/i.test(value)), true);
    const firstEffect = requests.findIndex(r => r.method !== 'GET');
    assert.ok(requests.slice(0, firstEffect).some(r => r.path === '/runtime-contract'));
    await hooks.get('session_shutdown')({ reason: 'quit' }, ctx);
  }, { version: '0.0.0' });
});

test('server identity change requalifies and rejects dispatch into a foreign runtime', async () => {
  await withRuntimeFixture(expected, async ({ ctx, tools, hooks, requests, effects, state }) => {
    await hooks.get('session_start')({}, ctx);
    const first = await tools.get('mem_stats').execute('initial-stats', {}, undefined, undefined, ctx);
    assert.notEqual(first.isError, true);
    const beforeEffects = effects.length;
    const beforeStats = requests.filter(r => r.path === '/stats').length;
    const beforeProbes = requests.filter(r => r.path === '/runtime-contract').length;
    state.identity = '2'.repeat(32);
    const result = await tools.get('mem_stats').execute('foreign-stats', {}, undefined, undefined, ctx);
    await hooks.get('session_shutdown')({ reason: 'quit' }, ctx);
    assert.equal(result.isError, true);
    assert.match(result.content[0].text, /identity changed/i);
    assert.equal(effects.length, beforeEffects);
    assert.equal(requests.filter(r => r.path === '/stats').length, beforeStats);
    assert.ok(requests.filter(r => r.path === '/runtime-contract').length > beforeProbes, 'identity is rechecked rather than trusted from cached readiness');
  });
});

test('mismatching runtime contract blocks native tools and lifecycle writes despite matching version', async () => {
  await withRuntimeFixture({ ...expected, digest: '0'.repeat(64) }, async ({ ctx, tools, hooks, requests, effects, statuses }) => {
    await hooks.get('session_start')({}, ctx);
    const result = await tools.get('mem_stats').execute('runtime-mismatch', {}, undefined, undefined, ctx);
    await hooks.get('session_shutdown')({ reason: 'quit' }, ctx);
    assert.equal(result.isError, true, 'a mismatching contract must fail closed');
    assert.match(result.content[0].text, /runtime contract/i);
    assert.match(result.content[0].text, /upgrade|update|restart/i);
    assert.deepEqual(effects, [], 'rejected qualification must leave all session/memory counters unchanged');
    assert.equal(requests.some(r => r.path === '/stats'), false, 'no native operation may dispatch before qualification');
    assert.equal(statuses.some(value => typeof value === 'string' && /\bready\b/i.test(value)), false, 'no full-provider readiness claim before qualification');
  });
});
