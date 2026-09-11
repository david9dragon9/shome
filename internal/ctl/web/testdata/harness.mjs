// Test harness for the web console's rendering logic.
//
// The console is a single inline script with no build step, which keeps it
// trivial to serve and audit but means it would otherwise be entirely
// untested. This provides the smallest DOM that its render path touches --
// far less than a headless browser, and no dependency to install -- then
// drives render() with a real Dashboard payload produced by the Go side.
//
// Usage: node harness.mjs <console.js> <dashboard.json> <users.json> <audit.json>
import fs from 'node:fs';

const [, , scriptPath, dashPath, usersPath, auditPath] = process.argv;

const els = {};
const mkEl = id => els[id] ||= {
  id, innerHTML: '', textContent: '', value: '', checked: false, style: {},
  classList: {
    _s: new Set(),
    add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); },
    toggle(c, on) {
      if (on === undefined) { this._s.has(c) ? this._s.delete(c) : this._s.add(c); }
      else { on ? this._s.add(c) : this._s.delete(c); }
    },
    contains(c) { return this._s.has(c); },
  },
  onclick: null, dataset: {}, append() {}, remove() {},
  // <select> exposes its options; the limits table reads them to keep the
  // current selection across a refresh.
  options: [],
};
globalThis.document = {
  querySelector: s => mkEl(s.replace('#', '')),
  querySelectorAll: () => [],
  createElement: () => mkEl('tmp'),
  addEventListener() {},
};
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.window = globalThis;
globalThis.setInterval = () => 0;
globalThis.clearInterval = () => {};
// The limits table fetches; serve it the payload the real handler produces.
globalThis.__qos = JSON.parse(process.env.SHOME_QOS || 'null');
// Where the login node is. Served because the enrollment handout asks the
// controller for it rather than guessing from location.hostname, which is
// what it used to do.
globalThis.__login = JSON.parse(process.env.SHOME_LOGIN || 'null');
globalThis.fetch = async (path) => {
  if (globalThis.__qos && String(path).includes('/qos')) {
    return { ok: true, status: 200, json: async () => globalThis.__qos };
  }
  if (globalThis.__login && String(path).includes('/login')) {
    return { ok: true, status: 200, json: async () => globalThis.__login };
  }
  throw new Error('the harness makes no network calls');
};

let src = fs.readFileSync(scriptPath, 'utf8');
src = src.replace(/\nboot\(\);\s*$/, '\n'); // boot() would try to fetch
src += `
globalThis.__setState = (d,u,a,t,acc) => { dash=d; users=u; audit=a; tab=t; if(acc) access=acc; };
globalThis.__render = render;
globalThis.__mib = mib; globalThis.__pct = pct; globalThis.__gauge = gauge;
globalThis.__loadQoS = loadQoS;
globalThis.__showHandout = showHandout;
globalThis.__sshCommandFor = sshCommandFor;
globalThis.__setLogin = v => { globalThis.__login = v; };
globalThis.__spark = spark;
`;
new Function(src)();

const dash = JSON.parse(fs.readFileSync(dashPath, 'utf8'));
const users = JSON.parse(fs.readFileSync(usersPath, 'utf8'));
const audit = JSON.parse(fs.readFileSync(auditPath, 'utf8'));

let fails = 0;
const check = (name, cond, extra = '') => {
  console.log(`  ${cond ? 'ok  ' : 'FAIL'} ${name}${cond ? '' : '  ' + extra}`);
  if (!cond) fails++;
};

// Formatters. Unknown must never render as a plausible zero.
check('mib(-1) is a dash', __mib(-1) === '-');
check('mib(512) is 512M', __mib(512) === '512M', __mib(512));
check('mib(16384) is 16.0G', __mib(16384) === '16.0G', __mib(16384));
check('pct(-1) is a dash', __pct(-1) === '-');
check('pct(62.5) rounds to 63%', __pct(62.5) === '63%', __pct(62.5));
check('unknown gauge is visibly different', __gauge(-1).includes('g-unknown'));
check('90%+ gauge is bad', __gauge(95).includes('g-bad'));
check('70%+ gauge is warn', __gauge(75).includes('g-warn'));
check('low gauge is ok', __gauge(20).includes('g-ok'));
check('sparkline draws a polyline', __spark([{cpu:1},{cpu:50}], 'cpu', 'red').includes('polyline'));
check('sparkline survives one point', __spark([{cpu:1}], 'cpu', 'red').includes('<svg'));

// Every tab renders against a real payload without throwing.
const accessData = JSON.parse(process.env.SHOME_ACCESS || '{"keys":[],"codes":[]}');
for (const t of ['overview', 'nodes', 'jobs', 'access', 'audit']) {
  __setState(dash, users, audit, t, accessData);
  try { __render(); check(`render() on the "${t}" tab`, true); }
  catch (e) { check(`render() on the "${t}" tab`, false, e.message); }
}

__setState(dash, users, audit, 'overview', accessData);
__render();
const cards = document.querySelector('#nodecards').innerHTML;
check('node card names its node', cards.includes(dash.nodes[0].name));
check('node card offers actions', cards.includes('drain') && cards.includes('forget'));
check('tiles show the node count',
  document.querySelector('#tiles').innerHTML.includes(`${dash.totals.nodes_up}/${dash.totals.nodes}`));
check('a DOWN node is still listed',
  !dash.nodes.some(n => n.state === 'DOWN') || cards.includes('DOWN'));

// A job name is attacker-controlled: anyone who can submit can choose it.
__setState({ ...dash, jobs: [{ id: 1, label: '1', name: '<img src=x onerror=alert(1)>',
  user: 'u', state: 'RUNNING', elapsed: '0', cpus: 1, mem_mib: 1, live_mib: -1, node: 'n' }] },
  users, audit, 'overview');
__render();
const q = document.querySelector('#queue').innerHTML;
check('job names are HTML-escaped', !q.includes('<img src=x') && q.includes('&lt;img'));

// A node name reaches an inline onclick handler, so quoting matters there too.
__setState({ ...dash, nodes: [{ ...dash.nodes[0], name: "ev'il" }] }, users, audit, 'overview');
__render();
check('node names are escaped into handlers',
  !document.querySelector('#nodecards').innerHTML.includes("drainNode('ev'il')"));

// Access management: the console must surface machines and unused codes, and
// offer the same operations the CLI has -- no more, since it is a wrapper.
__setState(dash, users, audit, 'access', accessData);
__render();
const acc = document.querySelector('#accesstable').innerHTML;
check('access list shows a machine', acc.includes('machine'));
check('access list shows an unused code', acc.includes('unused code'));
check('a machine can be signed out', acc.includes('unenrollMachine'));
check('an unused code can be cancelled', acc.includes('cancelCode'));
const ut = document.querySelector('#usertable').innerHTML;
check('accounts offer enrollment', ut.includes('enrollUser'));
check('accounts offer sign-out-everywhere', ut.includes('unenrollAll'));
check('accounts still offer quarantine', ut.includes('quarantine'));

// A fingerprint reaches an inline handler, so quoting matters.
__setState(dash, users, audit, 'access',
  {keys:[{user:"ev'il", fingerprint:"SHA256:a'b", comment:"x'y", last_used:''}], codes:[]});
__render();
const evil = document.querySelector('#accesstable').innerHTML;
check('identities are escaped into handlers', !evil.includes("unenrollMachine('ev'il'"));

// Limits, driven through the same endpoint the CLI uses.
if (globalThis.__qos) {
  __setState(dash, users, audit, 'access', accessData);
  __render();
  // Cluster totals first: a separate layer from the per-account default, and
  // nothing to "reset to", so no reset button.
  document.querySelector('#qosuser').value = 'cluster';
  await globalThis.__loadQoS();
  let qt = document.querySelector('#qostable').innerHTML;
  check('limits table lists a limit', qt.includes('max-running-jobs'));
  check('limits table shows usage', !qt.includes('undefined'));
  check('a limit can be changed', qt.includes('setLimit'));
  check('cluster totals offer no reset', !qt.includes('clearLimit'));
  check('cluster totals are their own layer', qt.includes('cluster-wide') || qt.includes('not set'));

  // The per-account default is a third, separate view.
  document.querySelector('#qosuser').value = '__default__';
  await globalThis.__loadQoS();
  const dt = document.querySelector('#qostable').innerHTML;
  check('per-account default renders', dt.includes('max-running-jobs'));
  check('per-account default offers no reset', !dt.includes('clearLimit'));

  // Then an account, whose overrides can be reset back to the default.
  document.querySelector('#qosuser').value = 'someone';
  await globalThis.__loadQoS();
  qt = document.querySelector('#qostable').innerHTML;
  const hasOverride = Object.keys(__qos.overrides || {}).length > 0;
  check('an account override can be reset', !hasOverride || qt.includes('clearLimit'));
  check('the source of each limit is shown',
    qt.includes('per-account default') || qt.includes('this account'));
}


// ---- the enrollment handout -------------------------------------------
//
// This is what an admin sees after clicking "enroll a machine". It used to
// build the ssh line from location.hostname with a hardcoded port, falling
// back to a literal "<controller>" -- which is exactly what it produced when
// the console was reached on loopback, i.e. by default. The address now comes
// from the controller.
{
  const handout = () => document.querySelector('handout').innerHTML;

  await __showHandout('alice', 'CODE123', '1h0m0s');
  check('handout names a real address', handout().includes('192.0.2.10'),
    handout());
  check('handout never shows a placeholder', !handout().includes('controller>'),
    handout());
  check('handout uses the configured port', handout().includes('ssh -p 2222'),
    handout());
  check('handout shows the code', handout().includes('CODE123'));
  check('handout keeps the command and the code together',
    /ssh -p 2222 alice@192\.0\.2\.10\nenrollment code: CODE123/.test(handout()),
    handout());

  // An IPv6 address has to be bracketed and quoted, or zsh refuses the line
  // with "no matches found".
  const v6 = __sshCommandFor('alice', '2001:db8::1', 2222);
  check('ipv6 handout is bracketed', v6.includes('[2001:db8::1]'), v6);
  check('ipv6 handout is quoted for the shell', v6.includes("'"), v6);

  // Several interfaces: alternates offered, but after the code.
  __setLogin({ enabled: true, port: 2222,
    hosts: ['192.0.2.10', '2001:db8::1', '2001:db8::2', '2001:db8::3'] });
  await __showHandout('alice', 'CODE123', '1h0m0s');
  check('alternates are offered when there are several addresses',
    handout().includes('also work') && handout().includes('2001:db8::1'), handout());
  check('alternates come after the code',
    handout().indexOf('CODE123') < handout().indexOf('also work'), handout());
  check('alternates are capped',
    !handout().includes('2001:db8::3'), handout());

  // No login node: an enrollment code has nowhere to be used, and saying so
  // beats printing instructions that cannot work.
  __setLogin({ enabled: false, port: 0, hosts: [] });
  await __showHandout('alice', 'CODE123', '1h0m0s');
  check('a disabled login node is reported', handout().includes('nowhere to ssh'),
    handout());
  check('the code is still shown when there is nowhere to use it',
    handout().includes('CODE123'), handout());
  check('no ssh line is invented when there is no login node',
    !handout().includes('ssh -p'), handout());

  // Reachable only on loopback.
  __setLogin({ enabled: true, port: 2222, hosts: [] });
  await __showHandout('alice', 'CODE123', '1h0m0s');
  check('an unreachable controller is reported', handout().includes('unreachable'),
    handout());
}

console.log(fails ? `\n${fails} failure(s)` : '\nall checks passed');
process.exit(fails ? 1 : 0);
