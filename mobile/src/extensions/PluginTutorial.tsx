import {useState} from 'react'
import {Image,Pressable,Text,View} from 'react-native'
import {PlayCircle} from 'lucide-react-native'
import {useVideoPlayer,VideoView} from 'expo-video'
import {useEvent} from 'expo'
import type {PluginTutorial as Tutorial} from '../../../frontend/src/api/extensions'
import {selectTutorial} from '../../../frontend/src/extensions/catalog'
import type {Controller} from '../api'
import {useLocale} from '../locale'
import {Hint,s} from '../ui'
export function PluginTutorial({api,tutorials}:{api:Controller;tutorials?:Tutorial[]}){
 const {language,t}=useLocale(),tutorial=selectTutorial(tutorials,language),[playing,setPlaying]=useState(false)
 if(!tutorial)return null
 const uri=new URL(tutorial.video,api.profile.server).href
 return <View style={{gap:12,marginVertical:10}}><Text style={[s.body,{fontWeight:'600'}]}>{t('Video guide')}</Text><Text style={s.hint}>{tutorial.title}</Text>
  {playing ? <Player key={uri} uri={uri} /> : <Pressable accessibilityRole="button" accessibilityLabel={t('Play video guide')} onPress={()=>setPlaying(true)} style={{aspectRatio:16/9,backgroundColor:'#e9edf3',borderRadius:18,overflow:'hidden',justifyContent:'center',alignItems:'center'}}>
   {tutorial.poster && <Image source={{uri:new URL(tutorial.poster,api.profile.server).href}} style={{position:'absolute',width:'100%',height:'100%'}} resizeMode="contain" />}<View style={{position:'absolute',inset:0,backgroundColor:'#15202f45'}} /><PlayCircle size={42} color="#fff" strokeWidth={1.5} /><Text style={[s.meta,{color:'#fff',marginTop:8}]}>{t('Play video guide')}</Text>
  </Pressable>}
 </View>
}
function Player({uri}:{uri:string}){
 const {t}=useLocale(),player=useVideoPlayer(uri,player=>player.play()),{status}=useEvent(player,'statusChange',{status:player.status})
 return <View style={{gap:10}}><VideoView player={player} surfaceType="textureView" nativeControls fullscreenOptions={{enable:true}} contentFit="contain" style={{width:'100%',aspectRatio:16/9,borderRadius:18,backgroundColor:'#182131'}} />{status==='error'&&<Hint error>{t('Could not load this video. Try again.')}</Hint>}</View>
}
