import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, writeFile, readFile, access, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Packages, validateSource } from '../src/packages.mjs'
import { HostRuntime } from '../src/host-runtime.mjs'

test('a native plugin package runs only approved native pnpm scripts, activates, withdraws and retains workspace files', async t => {
  const directory=await mkdtemp(join(tmpdir(),'go-bot-packages-'))
  t.after(()=>rm(directory,{recursive:true,force:true}))
  const source=join(directory,'source');const profile=join(directory,'profile')
  await mkdir(source)
  await mkdir(profile)
  const name='@fixture/inventory'
  await writeFile(join(source,'package.json'),JSON.stringify({name,version:'1.0.0',type:'module',exports:'./index.mjs',files:['index.mjs'],
    peerDependencies:{'@go-bot/plugin-sdk':'^1.0.0'},
    scripts:{postinstall:'node -e "require(\'fs\').writeFileSync(\'built.txt\',\'built\')"'},
    goBot:{api:'1'}}))
  await writeFile(join(source,'index.mjs'),`import {HostError,apiVersion} from '@go-bot/plugin-sdk';export const inject=['host'];export async function apply(ctx){await ctx.host.tools.register({name:'inventory_lookup',description:'Inventory',inputSchema:{type:'object'},async execute(){if(apiVersion!=='1')throw new HostError('VERSION','Wrong SDK');return{content:[{type:'text',text:'available'}],data:{available:true}}}})}`)
  const manager=new Packages({directory:profile})
  const declared={type:'directory',spec:source}
  assert.equal((await manager.inspect(name,declared)).version,'1.0.0')
  await assert.rejects(manager.install(name,declared), /Ignored build scripts/ )
  const installed=await manager.install(name,declared,{builds:[name]})
  assert.equal(installed.name,name)
  assert.equal(installed.api,'1')
  assert.equal((await manager.installed()).length,1)
  assert.equal((await manager.installed())[0].present,true)
  assert.equal(await readFile(join(profile,'node_modules',name,'built.txt'),'utf8'),'built')
  const contributions=new Map()
  const runtime=await HostRuntime.create({directory:profile,call:async(method,p)=>{
    const id=p.owner+'/'+p.callbackId
    if(method==='contribution.register'){contributions.set(id,p);return{id}}
    if(method==='contribution.remove')contributions.delete(p.registrationId)
    return null
  }})
  t.after(()=>runtime.close())
  const state=await runtime.apply({instances:[{id:'inventory',package:name,enabled:true}]})
  assert.equal(state.instances[0].state,'ready')
  assert.equal(contributions.size,1)
  const contribution=[...contributions.values()][0]
  const output=await runtime.invoke({owner:contribution.owner,callbackId:contribution.callbackId,method:'execute',args:{},context:{}},new AbortController().signal)
  assert.deepEqual(output.data,{available:true})
  await runtime.apply({instances:[]})
  assert.equal(contributions.size,0)
  await runtime.close()
  const data=join(directory,'data','inventory')
  await mkdir(data,{recursive:true})
  await writeFile(join(data,'business.json'),'{"retained":true}')
  await manager.remove(name)
  assert.equal((await manager.installed()).length,0)
  assert.equal(await readFile(join(data,'business.json'),'utf8'),'{"retained":true}')
})

test('package sources reject credentials in URLs, floating persisted versions and command-shaped input',()=>{
  for(const [name,source]of [
    ['../escape',{type:'directory',spec:'/tmp/pkg'}],
    ['valid',{type:'directory',spec:'relative'}],
    ['valid',{type:'registry',spec:'another@1.0.0'}],
    ['valid',{type:'registry',spec:'valid@latest'}],
    ['valid',{type:'registry',spec:'valid@1.0.0',registry:'https://secret@registry.example.com'}],
    ['valid',{type:'git',spec:'git+https://example.com/repo.git#main'}],
    ['valid',{type:'tarball',spec:'https://example.com/pkg.tgz?token=secret'}],
  ])assert.throws(()=>validateSource(name,source,{fixed:true}))
  assert.doesNotThrow(()=>validateSource('@fixture/provider',{type:'registry',spec:'@fixture/provider@1.2.3'},{fixed:true}))
  assert.doesNotThrow(()=>validateSource('valid',{type:'git',spec:'git+https://example.com/repo.git#'+'a'.repeat(40)},{fixed:true}))
})
