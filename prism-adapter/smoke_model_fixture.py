"""Synthetic model picker for local Chromium tests; no upstream data."""

PICKER = '''
<button id="picker" aria-haspopup="menu" aria-label="Thinking: 5.6 Sol Medium"
 onclick="document.getElementById('options').hidden=false">5.6 Sol\nMedium</button>
<div id="options" hidden>
 <button role="menuitem" onmouseenter="submenu('models')">Model</button>
 <button role="menuitem" onmouseenter="submenu('efforts')">Effort</button>
 <div id="models" hidden></div><div id="efforts" hidden></div>
</div>
<script>
window.currentModel='gpt-6.1-sol'; window.currentEffort='medium';
const modelLabels={'gpt-6.1-sol':'6.1 Sol'};
const effortLabels={low:'Low',medium:'Medium',high:'High',xhigh:'Extra high'};
const fixtureUser={userID:'synthetic-fixture-user',custom:{beta_program_enrolled:true}};
let catalogReady=false;
window.__STATSIG__={instances:{fixture:{
 loadingStatus:'Ready',
 getContext:()=>({user:fixtureUser}),
 updateUserAsync:async (user)=>{
  if(user!==fixtureUser)throw new Error('user attributes were replaced');
  catalogReady=true;
  document.querySelectorAll('#models button').forEach(e=>e.hidden=false);
  currentModel='gpt-6.1-sol'; selected();
 }
}}};
function submenu(id) {
 for(const name of ['models','efforts']) document.getElementById(name).hidden=name!==id;
}
function selected() {
 const label=modelLabels[currentModel]+'\\n'+effortLabels[currentEffort];
 document.getElementById('picker').innerText=label;
 document.getElementById('picker').setAttribute('aria-label','Thinking: '+label.replace('\\n',' '));
 document.getElementById('options').hidden=true;
 for(const id of ['models','efforts']) document.getElementById(id).hidden=true;
}
for(const [id,labels] of [['models',modelLabels],['efforts',effortLabels]]) {
 for(const [value,label] of Object.entries(labels)) {
  const option=document.createElement('button'); option.setAttribute('role','menuitem');
   option.hidden=id==='models' && value!=='gpt-6.1-sol' && !catalogReady;
  option.innerText=label; option.onclick=()=>{
   if(id==='models')window.currentModel=value; else window.currentEffort=value;
   selected();
  };
  document.getElementById(id).appendChild(option);
 }
}
</script>'''
