const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
function load(name, ctx) {
  let start = html.indexOf(`function ${name}(`);
  assert.notEqual(start, -1, name);
  if (html.slice(start - 6, start) === 'async ') start -= 6;
  const end = html.indexOf('\n}', start) + 2;
  vm.runInNewContext(html.slice(start, end), ctx);
}
function elements() {
  return new Proxy({}, {get(target, id) {
    assert.ok(!String(id).startsWith('fo-'), 'obsolete Failover control read: ' + String(id));
    return target[id] ||= {value:'', checked:false, textContent:'', reportValidity:()=>true};
  }});
}
test('settings removes Failover controls and saves only the Hitchance policy', async () => {
  assert.doesNotMatch(html, /id="fo-|STATE\.failover|onFoModeChange|<legend>Failover<\/legend>/);
  const els = elements();
  const policy = {enabled:true, retry_cycles:3, response_start_timeout_seconds:35, upstream_cache_ttl_hours:12, sticky_upstream_ttl_hours:8, invalid_confirmations:1, confirmation_window_seconds:86400, max_cooldown_seconds:86400, cooling_retry_seconds:10, honor_retry_after:true, rules:[]};
  const ctx = {$:id=>els[id], STATE:{server:{admin:{}}, usage:{}, hitchance:policy, model_discovery:{enabled:false}, auto_models_discovery:true}, renderProxies:()=>{}, refreshState:async()=>{}, toast:()=>{}, renderHitchanceRules:()=>{}};
  for(const name of ['hcNumberFields', 'loadHitchanceEditor', 'fillHitchance', 'readHitchancePolicy', 'fillSettings']) load(name, ctx);
  ctx.fillSettings();
  let sent;
  ctx.api = async (route, opts) => { assert.equal(route, 'settings'); sent=JSON.parse(opts.body); return {ok:'saved'}; };
  const start = html.indexOf("$('save-settings').onclick = async () => {");
  const end = html.indexOf('\n};', start) + 3;
  vm.runInNewContext(html.slice(start, end), ctx);
  await els['save-settings'].onclick();
  assert.ok(sent, 'settings handler submitted');
  assert.equal('failover' in sent, false);
  assert.deepEqual(sent.hitchance, policy);
  // The old auto-discovery switch is folded into the one "refresh model lists" option.
  assert.equal(els['md-enabled'].checked, true);
  assert.equal(sent.model_discovery.enabled, true);
  assert.equal(sent.auto_models_discovery, false);
});

function upstreamsContext(els) {
  const states = {a:'available', b:'ready', c:'degraded', d:'unavailable', e:'disabled'};
  const ctx = {
    $:id=>els[id], STATE:{upstreams:Object.keys(states).map(id=>({id, enabled:id!=='e', base_url:'https://example.invalid', models:['model'], api_keys:['sk-first-key', 'sk-second-key']}))},
    UPSTREAM_HEALTH:Object.fromEntries(Object.entries(states).map(([id,status])=>[id,{id,status,status_reason:'<img src=x onerror=alert(1)>'}])),
    UPSTREAM_HEALTH_ERROR:false, UPSTREAM_HEALTH_LOADING:false, selectedUpstreams:new Set(), UP_OPEN_KEYS:new Set(), UP_OPEN_MODELS:new Set(),
    getProxyTag:()=>'', maskKeyShort:k=>String(k).slice(0, 9), fmtTime:x=>x, updateUpSelectionUI:()=>{},
    esc:s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))
  };
  for(const name of ['upstreamHealthView', 'fmtTimeLeft', 'hitchanceStateText', 'hitchanceModelsText', 'hitText', 'hitHtml', 'upstreamHitchanceLines', 'upKeysDetail', 'upModelsDetail', 'renderUps']) load(name, ctx);
  return ctx;
}

test('upstream status labels distinguish eligibility from verified health and escape details', () => {
  const els = elements();
  const ctx = upstreamsContext(els);
  ctx.renderUps();
  const output=els['ups-body'].innerHTML;
  for(const label of ['Available','Ready','Degraded','Unavailable','Disabled']) assert.ok(output.includes(label),label);
  assert.ok(output.includes('no model response has been confirmed yet'));
  assert.doesNotMatch(output, /[\u0400-\u04FF]/, 'panel is English only');
  assert.equal(output.includes('<img'),false,'escaped server detail');
  assert.ok(output.includes('&lt;img'));
  assert.equal(ctx.upstreamHealthView(ctx.STATE.upstreams[1]).className,'warn','ready is not proven green');
  ctx.UPSTREAM_HEALTH_ERROR=true;
  assert.equal(ctx.upstreamHealthView(ctx.STATE.upstreams[0]).label,'No data','poll failure is not a health result');
  assert.equal(ctx.upstreamHealthView(ctx.STATE.upstreams[4]).label,'Disabled');
});

test('hitchance is shown separately for the API base and for each API key', () => {
  const els = elements();
  const ctx = upstreamsContext(els);
  const soon = new Date(Date.now() + 60000).toISOString();
  ctx.UPSTREAM_HEALTH.c = {id:'c', status:'degraded',
    endpoint_state:{action:'demote', category:'transient', rule_id:'server-outage', until:soon},
    keys:[{index:0}, {index:1, state:{action:'quarantine', category:'invalid_key', rule_id:'invalid-credential', failures:2}, models:{'<m>':{action:'demote', category:'model', until:soon}}}]};
  ctx.renderUps();
  let output = els['ups-body'].innerHTML;
  assert.match(output, /API base<\/span>: cooling down \d+s \(transient, rule server-outage\)/);
  assert.match(output, /API key 2<\/span> <span class="mono">sk-second<\/span>: quarantined until reset \(invalid_key, rule invalid-credential\), 2 failures; 1 model limited: &lt;m&gt;/);
  assert.doesNotMatch(output, /API key 1/, 'healthy keys are not listed inline');
  ctx.UP_OPEN_KEYS.add('c');
  ctx.renderUps();
  output = els['ups-body'].innerHTML;
  assert.match(output, /class="up-scope">API key 1<[\s\S]*?class="hc-on">OK</, 'expanded list shows every key');
  assert.match(output, /class="up-scope">API base<[\s\S]*?class="hc-warn">cooling down/);
});

test('hit chance % is shown next to the API base and next to each API key', () => {
  const els = elements();
  const ctx = upstreamsContext(els);
  assert.deepEqual([0.795, 0.4, 0.39].map(c => ctx.hitText(c, 10).text + ' ' + ctx.hitText(c, 10).level), ['80% good', '40% mid', '39% bad']);
  assert.equal(ctx.hitText(undefined, 0).text, '--%');
  ctx.UPSTREAM_HEALTH.a = {id:'a', status:'available', hit_chance:0.75, attempts:4,
    keys:[{index:0, hit_chance:1, attempts:3}, {index:1, hit_chance:0, attempts:1}]};
  ctx.renderUps();
  let output = els['ups-body'].innerHTML;
  assert.ok(output.includes('<span class="hit hit-mid" title="API base: 75% of the last 4 requests succeeded">75%</span>'), 'API base %');
  assert.ok(output.includes('<span class="key-hits">(<span class="hit hit-good" title="API key 1: 100% of the last 3 requests succeeded">100%</span><span class="sep"> · </span><span class="hit hit-bad" title="API key 2: 0% of the last 1 request succeeded">0%</span>)</span>'), 'per-key %');
  assert.ok(output.includes('title="API base: No requests yet">--%</span>'), 'an unused API base says so');
  assert.equal((output.match(/class="key-hits"/g) || []).length, 1, 'keys without requests add no list');
  assert.equal((output.match(/class="hit /g) || []).length, 4 + 2, 'four enabled API bases and two keys; the disabled upstream has no figure');
  ctx.UP_OPEN_KEYS.add('a');
  ctx.renderUps();
  output = els['ups-body'].innerHTML;
  assert.doesNotMatch(output, /class="key-hits"/, 'the expanded list replaces the inline figures');
  assert.match(output, /class="up-scope">API base<[\s\S]*?class="up-hit"><span class="hit hit-mid"[^>]*>75%/);
  assert.match(output, /class="up-scope">API key 1<[\s\S]*?class="up-hit"><span class="hit hit-good"[^>]*>100%/);
  assert.match(output, /class="up-scope">API key 2<[\s\S]*?class="up-hit"><span class="hit hit-bad"[^>]*>0%/);
});

test('health polling exposes API failures without erasing the last snapshot and recovers', async () => {
  const ctx={UPSTREAM_HEALTH:{a:{id:'a',status:'available'}}, UPSTREAM_HEALTH_ERROR:false, UPSTREAM_HEALTH_LOADING:false,
    UPSTREAM_HEALTH_RECEIVED:false, UPSTREAM_HEALTH_SIGNATURE:'', currentView:'upstreams', renderUps:()=>{}, AbortSignal, api:async()=>({error:'HTTP 500'})};
  load('refreshUpstreamHealth',ctx);
  await ctx.refreshUpstreamHealth();
  assert.equal(ctx.UPSTREAM_HEALTH_ERROR,true);
  assert.equal(ctx.UPSTREAM_HEALTH.a.status,'available');
  assert.equal(ctx.UPSTREAM_HEALTH_LOADING,false);
  ctx.api=async()=>({upstreams:[{id:'a',status:'ready'}]});
  await ctx.refreshUpstreamHealth();
  assert.equal(ctx.UPSTREAM_HEALTH_ERROR,false);
  assert.equal(ctx.UPSTREAM_HEALTH_RECEIVED,true);
  assert.equal(ctx.UPSTREAM_HEALTH.a.status,'ready');
  ctx.api=async()=>{throw Error('offline');};
  await ctx.refreshUpstreamHealth();
  assert.equal(ctx.UPSTREAM_HEALTH_ERROR,true);
  assert.equal(ctx.UPSTREAM_HEALTH.a.status,'ready');
});

test('health polling does not overlap requests', async () => {
  let resolve, calls=0;
  const response=new Promise(r=>{resolve=r;});
  const ctx={UPSTREAM_HEALTH:{}, UPSTREAM_HEALTH_ERROR:false, UPSTREAM_HEALTH_LOADING:false, UPSTREAM_HEALTH_RECEIVED:false,
    UPSTREAM_HEALTH_SIGNATURE:'', currentView:'upstreams', renderUps:()=>{}, AbortSignal, api:()=>{calls++; return response;}};
  load('refreshUpstreamHealth',ctx);
  const first=ctx.refreshUpstreamHealth();
  const second=ctx.refreshUpstreamHealth();
  assert.equal(calls,1);
  resolve({upstreams:[]});
  await Promise.all([first,second]);
  assert.equal(ctx.UPSTREAM_HEALTH_LOADING,false);
});

test('health polling re-renders only on a change or while a countdown runs', async () => {
  let renders = 0;
  const ctx={UPSTREAM_HEALTH:{}, UPSTREAM_HEALTH_ERROR:false, UPSTREAM_HEALTH_LOADING:false, UPSTREAM_HEALTH_RECEIVED:false,
    UPSTREAM_HEALTH_SIGNATURE:'', currentView:'upstreams', renderUps:()=>{renders++;}, AbortSignal, api:async()=>({upstreams:[{id:'a',status:'ready'}]})};
  load('refreshUpstreamHealth',ctx);
  await ctx.refreshUpstreamHealth();
  await ctx.refreshUpstreamHealth();
  assert.equal(renders,1,'unchanged health does not rebuild the table');
  ctx.api=async()=>({upstreams:[{id:'a',status:'degraded',endpoint_state:{action:'demote',until:'2999-01-01T00:00:00Z'}}]});
  await ctx.refreshUpstreamHealth();
  await ctx.refreshUpstreamHealth();
  assert.equal(renders,3,'a running cooldown keeps its countdown fresh');
});
