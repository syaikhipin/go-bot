import { Image, StyleSheet, View } from 'react-native'
import { Puzzle } from 'lucide-react-native'
import type { Controller } from '../api'
export function PluginIcon({ api, icon, large = false }: { api: Controller; icon?: string; large?: boolean }) {
 const size = large ? 64 : 48
 return <View accessible={false} style={[styles.mark,{width:size,height:size,borderRadius:large ? 18 : 14}]}>{icon ? <Image source={{uri:new URL(icon,api.profile.server).toString()}} style={{width:size,height:size}} /> : <Puzzle size={large ? 30 : 24} strokeWidth={1.65} color="#74809a" />}</View>
}
const styles = StyleSheet.create({mark:{alignItems:'center',justifyContent:'center',overflow:'hidden',backgroundColor:'#fff',borderWidth:1,borderColor:'#25334a12'}})
