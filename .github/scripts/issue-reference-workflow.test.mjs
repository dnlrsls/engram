import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const workflow = readFileSync(new URL('../workflows/pr-check.yml', import.meta.url), 'utf8').replace(/\r\n/g, '\n');
const contextRepo = { owner: 'dnlrsls', repo: 'engram' };
const approved = { title: 'Approved work', labels: [{ name: 'status:approved' }] };

function scriptFor(job) {
  const block = workflow.split(`  ${job}:\n`)[1]?.split(/\n  (?=\S)/)[0];
  assert.ok(block, `Missing public workflow job: ${job}`);
  const lines = block.split('\n');
  const start = lines.findIndex(line => line.trim() === 'script: |');
  assert.notEqual(start, -1);
  const body = [];
  for (const line of lines.slice(start + 1)) {
    if (line !== '' && !line.startsWith('            ')) break;
    body.push(line.slice(12));
  }
  return new (Object.getPrototypeOf(async function () {}).constructor)('github', 'context', 'core', body.join('\n'));
}

async function run(job, body, issues = {}) {
  const requests = [], info = [], errors = [];
  const before = JSON.stringify(issues);
  const github = { rest: { issues: { get: async request => {
    requests.push(request);
    const key = `${request.owner}/${request.repo}#${request.issue_number}`;
    if (issues[key] instanceof Error) throw issues[key];
    return { data: issues[key] ?? approved };
  } } } };
  await scriptFor(job)(github, { repo: contextRepo, payload: { pull_request: { body } } }, {
    info: message => info.push(message), setFailed: message => errors.push(message),
  });
  assert.equal(JSON.stringify(issues), before, 'Metadata validation must not mutate issue data');
  return { requests, info, errors };
}

const reference = 'check-issue-reference';
const approval = 'check-issue-approved';

// S1: each qualified reference must use its own repository, for every closing keyword.
for (const keyword of ['Closes', 'Fixes', 'Resolves']) {
  test(`${keyword} qualified upstream reference passes both public jobs`, async () => {
    const body = `${keyword} Gentleman-Programming/engram#1350`;
    const syntax = await run(reference, body);
    assert.deepEqual(syntax.errors, []);
    assert.deepEqual(syntax.requests, []);
    assert.deepEqual(syntax.info, ['✅ Found issue reference(s): Gentleman-Programming/engram#1350']);
    const result = await run(approval, body);
    assert.deepEqual(result.errors, []);
    assert.deepEqual(result.requests, [{ owner: 'Gentleman-Programming', repo: 'engram', issue_number: 1350 }]);
    assert.deepEqual(result.info, [
      'Checking issue Gentleman-Programming/engram#1350 for status:approved label...',
      'Issue Gentleman-Programming/engram#1350 labels: status:approved',
      '✅ Issue Gentleman-Programming/engram#1350 has status:approved',
    ]);
  });
}

// S2: previous bare-reference behavior and exact messages remain unchanged.
for (const keyword of ['Closes', 'Fixes', 'Resolves']) {
  test(`${keyword} bare reference preserves lookup and messages`, async () => {
    const body = `${keyword} #1501`;
    assert.deepEqual(await run(reference, body), { requests: [], errors: [], info: ['✅ Found issue reference(s): #1501'] });
    assert.deepEqual(await run(approval, body), {
      requests: [{ owner: 'dnlrsls', repo: 'engram', issue_number: 1501 }], errors: [],
      info: ['Checking issue #1501 for status:approved label...', 'Issue #1501 labels: status:approved', '✅ Issue #1501 has status:approved'],
    });
  });
}

test('qualified current repository is trusted, including case-insensitive identity', async () => {
  for (const body of ['Closes dnlrsls/engram#15', 'Closes DNLRSLS/ENGRAM#15']) {
    assert.deepEqual((await run(reference, body)).errors, []);
    const result = await run(approval, body);
    const [, owner, repo] = /Closes ([^/]+)\/([^#]+)/.exec(body);
    assert.deepEqual(result.errors, []);
    assert.deepEqual(result.requests, [{ owner, repo, issue_number: 15 }]);
  }
});

// S2: reject the whole batch before lookup, even if a trusted reference appears first.
for (const body of ['Closes attacker/engram#1350', 'Closes Gentleman-Programming/other#1350', 'Closes #15\nFixes attacker/engram#1350']) {
  test(`untrusted repository is rejected without lookup: ${JSON.stringify(body)}`, async () => {
    for (const job of [reference, approval]) {
      const result = await run(job, body);
      const denied = body.includes('Gentleman-Programming/other') ? 'Gentleman-Programming/other' : 'attacker/engram';
      assert.deepEqual(result.errors, [`❌ Issue reference uses an untrusted repository: ${denied}.`]);
      assert.deepEqual(result.requests, []);
      assert.deepEqual(result.info, []);
    }
  });
}

test('every mixed reference is looked up and must be approved', async () => {
  const body = 'Closes #15\nFixes Gentleman-Programming/engram#1350';
  const result = await run(approval, body, { 'Gentleman-Programming/engram#1350': { title: 'Needs review', labels: [] } });
  assert.deepEqual(result.requests, [
    { owner: 'dnlrsls', repo: 'engram', issue_number: 15 },
    { owner: 'Gentleman-Programming', repo: 'engram', issue_number: 1350 },
  ]);
  assert.deepEqual(result.errors, ['❌ Linked issue(s) not approved:\nIssue Gentleman-Programming/engram#1350 ("Needs review") does not have the `status:approved` label. A maintainer must approve the issue before a PR can be merged.']);
  assert.deepEqual(result.info, [
    'Checking issue #15 for status:approved label...', 'Issue #15 labels: status:approved', '✅ Issue #15 has status:approved',
    'Checking issue Gentleman-Programming/engram#1350 for status:approved label...', 'Issue Gentleman-Programming/engram#1350 labels: (none)',
  ]);
});

test('bare unapproved and fetch-error paths retain previous exact behavior', async () => {
  const result = await run(approval, 'Closes #15\nFixes #16', {
    'dnlrsls/engram#15': { title: 'Needs review', labels: [] }, 'dnlrsls/engram#16': new Error('not found'),
  });
  assert.deepEqual(result.requests, [
    { owner: 'dnlrsls', repo: 'engram', issue_number: 15 }, { owner: 'dnlrsls', repo: 'engram', issue_number: 16 },
  ]);
  assert.deepEqual(result.errors, ['❌ Linked issue(s) not approved:\nIssue #15 ("Needs review") does not have the `status:approved` label. A maintainer must approve the issue before a PR can be merged.\nIssue #16: could not be fetched (not found)']);
  assert.deepEqual(result.info, ['Checking issue #15 for status:approved label...', 'Issue #15 labels: (none)', 'Checking issue #16 for status:approved label...']);
});

test('no closing reference retains the existing exact errors and no lookup', async () => {
  assert.deepEqual(await run(reference, 'No reference'), {
    requests: [], info: [], errors: ['❌ PR must reference an approved issue.\nAdd "Closes #N", "Fixes #N", or "Resolves #N" to the PR body.\nSee CONTRIBUTING.md for the full contribution workflow.'],
  });
  assert.deepEqual(await run(approval, 'No reference'), { requests: [], info: [], errors: ['❌ No issue reference found in PR body.'] });
});
