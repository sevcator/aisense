const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html=fs.readFileSync(path.join(__dirname,'index.html'),'utf8');
function load(name,ctx){let start=html.indexOf(`function ${name}(`);assert.notEqual(start,-1,name);if(html.slice(start-6,start)==='async ')start-=6;const end=html.indexOf('\n}',start)+2;vm.runInNewContext(html.slice(start,end),ctx);}
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const policy=()=>({enabled:true,retry_cycles:2,response_start_timeout_seconds:20,upstream_cache_ttl_hours:24,sticky_upstream_ttl_hours:24,invalid_confirmations:2,confirmation_window_seconds:86400,max_cooldown_seconds:3600,cooling_retry_seconds:10,honor_retry_after:true,rules:[{id:'custom',messages:['bad key'],category:'invalid_key',action:'delete',scope:'key',cooldown_seconds:300}]});
test('graphical hitchance editor round-trips the complete policy and validates numbers',()=>{
 const els=new Proxy({},{get:(t,id)=>t[id]||={value:'',checked:false}});
 const ctx={$:id=>els[id],STATE:{hitchance:policy()},rendered:0};
 ctx.renderHitchanceRules=()=>{ctx.rendered++;};
 for(const name of ['hcNumberFields','loadHitchanceEditor','fillHitchance','readHitchancePolicy'])load(name,ctx);
 ctx.fillHitchance();
 assert.equal(els['hc-enabled'].checked,true);assert.equal(ctx.rendered,1);
 // Nothing is lost and nothing is added: the server rejects unknown fields.
 // JSON copies: objects made inside a vm context have that context's prototypes.
 assert.deepEqual(JSON.parse(JSON.stringify(ctx.readHitchancePolicy())),policy());
 ctx.STATE.hitchance.rules.push({id:'later'});
 assert.equal(ctx.readHitchancePolicy().rules.length,1,'editor works on a copy');
 els['hc-enabled'].checked=false;els['hc-confirmations'].value='3';
 const edited=ctx.readHitchancePolicy();
 assert.equal(edited.enabled,false);assert.equal(edited.invalid_confirmations,3);assert.equal(edited.max_cooldown_seconds,3600,'hidden policy values are preserved');
 for(const bad of ['','0','121','2.5','x']){els['hc-start-timeout'].value=bad;assert.throws(()=>ctx.readHitchancePolicy(),/Response start timeout/);}
});
test('rule checks, summaries and the rules table',()=>{
 const ctx={esc};
 for(const name of ['fmtDuration','hcScopeName','hcRuleWhen','hcRuleThen','hitchanceRuleProblem','renderHitchanceRules'])load(name,ctx);
 const ok={id:'r',category:'rate_limit',action:'demote',scope:'key',cooldown_seconds:60,status_codes:[429]};
 assert.equal(ctx.hitchanceRuleProblem(ok,[]),'');
 assert.equal(ctx.hitchanceRuleProblem({...ok,action:'ignore',cooldown_seconds:0},[]),'');
 for(const [rule,want,taken] of [
  [{...ok,id:''},/ID/],[ok,/already/,['r']],[{...ok,category:''},/category/],
  [{id:'r',category:'c',action:'demote',scope:'key',cooldown_seconds:60},/at least one/],
  [{...ok,status_codes:[200]},/400 to 599/],[{...ok,cooldown_seconds:0},/at least 1 second/],
  [{...ok,action:'delete'},/error text or a pattern/],[{...ok,action:'delete',scope:'endpoint',messages:['x']},/Only an API key/],
 ])assert.match(ctx.hitchanceRuleProblem(rule,taken||[]),want);
 assert.equal(ctx.hcRuleThen({action:'demote',scope:'endpoint',cooldown_seconds:30}),'Cool down the API base for 30s');
 assert.equal(ctx.hcRuleThen({action:'quarantine',scope:'model'}),'Quarantine the API key + model until reset');
 assert.equal(ctx.hcRuleWhen({status_codes:[401,403],messages:['a','b','c'],models:['m']}),'HTTP 401, 403 and text “a” or “b” or 1 more, only for model m');
 assert.equal(ctx.hcRuleWhen({match_any:true,status_codes:[402,429],messages:['quota'],pattern:'(?i)weekly',upstream_ids:['u1']}),'HTTP 402, 429 or text “quota” or text matching (?i)weekly, only for upstream u1');
 assert.equal(ctx.hcRuleWhen({pattern:'(?i)(model.*(not included|not allowed|access))'}),'a text pattern','long patterns stay in the editor');
 assert.equal(ctx.hcRuleThen({action:'delete',scope:'key',cooldown_seconds:300,match_any:true,status_codes:[401],messages:['bad key']}),'Delete the API key (cool down 5m until confirmed; a status code alone only cools it down)');
 assert.equal(ctx.hcRuleThen({action:'delete',scope:'key',cooldown_seconds:300,messages:['bad key']}),'Delete the API key (cool down 5m until confirmed)');
 assert.equal(ctx.fmtDuration(3600),'1h');assert.equal(ctx.fmtDuration(90),'90s');
 const els={'hc-rules':{innerHTML:''}};
 ctx.$=id=>els[id];ctx.HC_POLICY={rules:[{id:'<img src=x>',category:'c',action:'demote',scope:'key',cooldown_seconds:5,messages:['<b>']}]};
 ctx.renderHitchanceRules();
 assert.doesNotMatch(els['hc-rules'].innerHTML,/<img|<b>/);assert.match(els['hc-rules'].innerHTML,/&lt;img/);
});
test('the rule editor keeps "any" and "all" rules exactly and starts new rules with "any"',()=>{
 const els=new Proxy({},{get:(t,id)=>t[id]||={value:'',checked:false,disabled:false,textContent:'',innerHTML:'',querySelectorAll:()=>[]}});
 const rules=[
  {id:'key-limit',category:'limit',match_any:true,status_codes:[402,429],messages:['rate limit','quota'],pattern:'(?i)weekly.*limit',action:'demote',scope:'key',cooldown_seconds:60},
  {id:'strict',category:'auth',status_codes:[401],messages:['bad key'],action:'delete',scope:'key',cooldown_seconds:300},
 ];
 const ctx={$:id=>els[id],esc,STATE:{upstreams:[]},HC_POLICY:{rules},hcEditingIndex:-1,openModal:()=>{}};
 for(const name of ['fmtDuration','hcScopeName','hcRuleThen','describeHitchanceRule','editHitchanceRule','hcRuleFromForm'])load(name,ctx);
 for(const [i,rule] of rules.entries()){ctx.editHitchanceRule(i);assert.equal(els['hc-rule-match'].value,rule.match_any?'any':'all');assert.deepEqual(JSON.parse(JSON.stringify(ctx.hcRuleFromForm())),rule);}
 ctx.editHitchanceRule(-1);
 assert.equal(els['hc-rule-match'].value,'any');
 assert.match(els['hc-rule-summary'].textContent,/^Cool down the API key for 1m\./);
});
test('hitchance keeps only the requested controls and rule editor',()=>{
 for(const id of ['hc-rules','overlay-hc-rule','hc-enabled','hc-start-timeout','hc-confirmations'])assert.ok(html.includes(`id="${id}"`),id);
 for(const id of ['hc-retry-cycles','hc-cache-ttl','hc-sticky-ttl','hc-confirm-window','hc-max-cooldown','hc-cooling-retry','hc-honor-retry','hc-preview-body','hc-preview-status','hc-preview-result'])assert.ok(!html.includes(`id="${id}"`),id);
 assert.doesNotMatch(html,/id="hc-policy"/,'raw JSON editor replaced by the graphical editor');
 assert.ok(html.includes("api('hitchance/reset'"));
});
