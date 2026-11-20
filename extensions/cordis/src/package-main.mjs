import { Packages } from './packages.mjs'
import { RuntimeError } from './errors.mjs'

let data=''
try{
  for await(const chunk of process.stdin){data+=chunk;if(data.length>4<<20)throw new RuntimeError('INPUT_LIMIT','Package request exceeds 4 MiB')}
  const request=JSON.parse(data)
  const manager=new Packages({directory:request.directory,pnpm:request.pnpm,credentials:request.credentials})
  let result
  switch(request.method){
    case 'inspect':result=await manager.inspect(request.name,request.source);break
    case 'install':result=await manager.install(request.name,request.source,{builds:request.builds??[]});break
    case 'remove':result=await manager.remove(request.name);break
    case 'list':result=await manager.installed();break
    default:throw new RuntimeError('METHOD_NOT_FOUND','Unknown package operation')
  }
  process.stdout.write(JSON.stringify({result})+'\n')
}catch(error){
  let message=String(error?.message??error)
  try{for(const secret of Object.values(JSON.parse(data).credentials??{}))if(secret)message=message.replaceAll(secret,'[redacted]')}catch{}
  process.stdout.write(JSON.stringify({error:{code:typeof error?.code==='string'?error.code:'PACKAGE_ERROR',message:message.slice(0,6000)}})+'\n')
  process.exitCode=1
}
