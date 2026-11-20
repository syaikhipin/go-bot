// Run the unedited pinned model-selection.ts module compiled to ESM. This is
// only a projection/API oracle; it never starts a Node Agent or Session owner.
import {readFileSync} from 'node:fs'
import {pathToFileURL} from 'node:url'

const {AgentModel,modelSelectionProjectionDefinition:projection}=await import(pathToFileURL(process.argv[2]).href)
const actions=JSON.parse(readFileSync(0,'utf8'))
let state=projection.init(),options={},seq=0,selectionEvents=0
const append=(type,data)=>{
  state=projection.apply(state,{type,data,seq:seq++,time:1})
  if(type==='model/selection')selectionEvents++
}
const model=new AgentModel(()=>state,{append},{
  get provider(){return options.provider},get model(){return options.model},get reasoningEffort(){return options.reasoningEffort},
})
append('turn/start',{turn:1})
append('step/start',{turn:1,step:1})
const snapshots=[]
for(const action of actions){
  if(action.kind==='options')options=action.value
  else if(action.kind==='select')model.select(action.value)
  else if(action.kind==='header')append('request/header',{header:action.value})
  else if(action.kind==='clear')append('model/selection',null)
  else if(action.kind!=='read')throw new Error('Unknown oracle action')
  snapshots.push({current:model.current()??null,state:structuredClone(state),seq,selectionEvents})
}
process.stdout.write(JSON.stringify(snapshots,null,2)+'\n')
