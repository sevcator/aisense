const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
function loadFunction(name, context) {
  const start = html.indexOf(`function ${name}(`);
  assert.notEqual(start, -1);
  const end = html.indexOf('\n}', start) + 2;
  vm.runInNewContext(html.slice(start, end), context);
}
test('both pages have syntactically valid JavaScript and an animated top divider', () => {
  for (const file of ['index.html', 'login.html']) {
    const text = fs.readFileSync(path.join(__dirname, file), 'utf8');
    for (const match of text.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)) new vm.Script(match[1]);
    assert.match(text, /\.top-divider\{position:sticky;top:0;/);
    assert.match(text, /divider-flow \d+(\.\d+)?s linear infinite/);
    // Only the colours flow: no white highlight sweeps across the divider.
    assert.doesNotMatch(text, /divider-shine|\.top-divider::after/);
    // Reduced motion only slows the divider down; it must never switch the animation off.
    const reduced = text.match(/@media\(prefers-reduced-motion:reduce\)\{((?:[^{}]*\{[^{}]*\})*)\s*\}/);
    assert.ok(reduced, 'reduced-motion rule');
    assert.doesNotMatch(reduced[1], /animation:none/);
  }
});
test('popups are in-page modals, never native browser dialogs', () => {
  assert.doesNotMatch(html, /(^|[^.\w])(confirm|alert|prompt)\(/m);
  assert.doesNotMatch(html, /<dialog|showModal\(/);
  for (const id of ['overlay-confirm', 'confirm-message', 'confirm-ok', 'toast-stack']) assert.ok(html.includes(`id="${id}"`), id);
  assert.match(html, /function uiConfirm\(/);
});
test('tasks mutually exclude, clear others, and allow unchecking to choose another', () => {
  const ids = ['test-delete', 'test-disable', 'test-hide'];
  const elements = Object.fromEntries(ids.map(id => [id, {checked:false, disabled:false}]));
  const context = {$: id => elements[id]};
  loadFunction('updateTestTasks', context);
  for (const active of ids) {
    elements[active].checked = true;
    context.updateTestTasks();
    for (const id of ids) {
      assert.equal(elements[id].checked, id === active);
      assert.equal(elements[id].disabled, id !== active);
    }
    elements[active].checked = false;
    context.updateTestTasks();
    for (const id of ids) assert.equal(elements[id].disabled, false);
  }
  elements['test-delete'].checked = elements['test-hide'].checked = true;
  context.updateTestTasks();
  assert.equal(elements['test-hide'].checked, false);
});
test('one Add dialog switches single and batch fields and actions', () => {
  const elements = Object.fromEntries(['up-single-fields', 'up-batch-fields', 'up-save-btn', 'up-test-btn', 'up-import-btn'].map(id => [id, {}]));
  const context = {$: id => elements[id]};
  loadFunction('setUpAddMode', context);
  context.setUpAddMode('batch');
  assert.equal(elements['up-single-fields'].hidden, true);
  assert.equal(elements['up-import-btn'].hidden, false);
  context.setUpAddMode('single');
  assert.equal(elements['up-single-fields'].hidden, false);
  assert.equal(elements['up-import-btn'].hidden, true);
  assert.equal((html.match(/onclick="upModal\(\)">Add<\/button>/g) || []).length, 1);
  for (const obsolete of ['ups-test-results', 'ups-test-summary', 'ups-test-live-body', 'overlay-up-import']) assert.equal(html.includes(obsolete), false);
});
test('upstream keys use one numbered multiline editor', () => {
  assert.match(html, /<textarea id="u-keys"[^>]*rows="8"/);
  assert.doesNotMatch(html, /upstream-key-input|addUpstreamKey\(|removeUpstreamKey\(/);
  const elements = {'u-keys': {value: ''}};
  const context = {$: id => elements[id]};
  for (const name of ['formatUpstreamKeys', 'parseUpstreamKeys', 'renumberUpstreamKeys', 'renderUpstreamKeyInputs']) loadFunction(name, context);
  context.renderUpstreamKeyInputs(['sk-first', 'sk-second']);
  assert.equal(elements['u-keys'].value, 'sk-first # 1\nsk-second # 2');
  elements['u-keys'].value = '  sk-first # 1\r\nsk-second # 2\n\nsk-third#literal\n';
  assert.deepEqual(Array.from(context.parseUpstreamKeys(elements['u-keys'].value)), ['sk-first', 'sk-second', 'sk-third#literal']);
  context.renumberUpstreamKeys();
  assert.equal(elements['u-keys'].value, 'sk-first # 1\nsk-second # 2\nsk-third#literal # 3');
});
test('empty installation identifies the active settings file', () => {
  const elements = {'hdr-summary': {}, 'setup-notice': {}, 'setup-config-path': {}};
  const context = {
    $: id => elements[id], STATE: {upstreams: [], api_keys: [], proxies: {list: []}, config_path: 'C:\\sandbox\\config.json'}, currentView: 'dashboard',
    syncUsageVisibility:()=>{}, renderKeys:()=>{}, renderUps:()=>{}, renderCombos:()=>{}, fillSettings:()=>{}, renderDashboardModels:()=>{}, loadStats:async()=>{}
  };
  loadFunction('renderAll', context);
  context.renderAll();
  assert.equal(elements['setup-notice'].hidden, false);
  assert.equal(elements['setup-config-path'].textContent, 'C:\\sandbox\\config.json');
  context.STATE.upstreams = [{id: 'up'}];
  context.renderAll();
  assert.equal(elements['setup-notice'].hidden, true);
});
test('the combos tab shows the model chain and refuses unusable combos', () => {
  for (const id of ['v-combos', 'combos-body', 'overlay-combo', 'combo-name', 'combo-models', 'combo-pick']) assert.ok(html.includes(`id="${id}"`), id);
  assert.match(html, /data-v="combos"/, 'combos is reachable from the menu');
  const elements = {'combos-body': {innerHTML: ''}};
  const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const context = {$: id => elements[id], esc, STATE: {combos: [{name: 'favourite', models: ['kimi-k3', 'glm-5.3', 'glm-5.3-flash']}]}};
  for (const name of ['renderCombos', 'comboModelList', 'comboProblem']) loadFunction(name, context);
  context.renderCombos();
  assert.match(elements['combos-body'].innerHTML, /favourite[\s\S]*kimi-k3[\s\S]*glm-5\.3[\s\S]*glm-5\.3-flash/, 'models keep their order');
  assert.match(elements['combos-body'].innerHTML, /delCombo\(&quot;favourite&quot;\)/, 'the name is escaped into the handler');
  context.STATE.combos = [];
  context.renderCombos();
  assert.match(elements['combos-body'].innerHTML, /No combos yet/);
  context.STATE.combos = [{name: '<img src=x>', models: ['<b>']}];
  context.renderCombos();
  assert.doesNotMatch(elements['combos-body'].innerHTML, /<img|<b>/, 'combo names and models are escaped');
  assert.deepEqual(Array.from(context.comboModelList(' kimi-k3 , glm-5.3 ,, ')), ['kimi-k3', 'glm-5.3']);
  assert.equal(context.comboProblem('favourite', ['kimi-k3']), '');
  for (const [name, models, want] of [['', ['m'], /name/], ['my combo', ['m'], /spaces/], ['favourite', [], /at least one/], ['favourite', ['Favourite'], /itself/]]) {
    assert.match(context.comboProblem(name, models), want);
  }
});
test('settings keep one control per job', () => {
  assert.doesNotMatch(html, /id="auto-models-discovery"|Enable auto models discovery|Periodic refresh is optional/);
  assert.equal((html.match(/id="md-enabled"/g) || []).length, 1);
  assert.doesNotMatch(html, /overlay-proxy-checker|openProxyCheckerModal|overlay-access/);
  for (const id of ['p-add', 'p-check-btn', 'p-add-btn']) assert.ok(html.includes(`id="${id}"`), id);
});
