import { useRef, useState, type ComponentRef } from 'react'
import { Keyboard, Modal, Platform, Pressable, StyleSheet, Text, useWindowDimensions, View } from 'react-native'
import { Ellipsis, Folder, Puzzle } from 'lucide-react-native'
import { useLocale } from './locale'
import { edge, IconButton, ink, s } from './ui'

export function ChatMenu({ onFiles, onPlugins }: { onFiles: () => void; onPlugins: () => void }) {
 const { t } = useLocale(), screen = useWindowDimensions(), anchor = useRef<ComponentRef<typeof View>>(null)
 const [position, setPosition] = useState<{ x: number; y: number }>()
 const pending = useRef<(() => void) | undefined>(undefined)
 const finish = () => { const action = pending.current; pending.current = undefined; action?.() }
 const choose = (action: () => void) => { pending.current = action; setPosition(undefined); if (Platform.OS !== 'ios') requestAnimationFrame(finish) }
 return <View ref={anchor} collapsable={false}>
  <IconButton label={t('Workspace menu')} accessibilityState={{ expanded: !!position }} onPress={() => { Keyboard.dismiss(); anchor.current?.measureInWindow((x, y, width, height) => setPosition({ x: Math.max(16, Math.min(x + width - 206, screen.width - 222)), y: Math.max(16, Math.min(y + height + 8, screen.height - 144)) })) }}><Ellipsis size={23} color={ink} strokeWidth={1.75} /></IconButton>
  <Modal visible={!!position} transparent animationType="none" onDismiss={finish} onRequestClose={() => setPosition(undefined)} supportedOrientations={['portrait', 'landscape']}>
   <Pressable accessibilityLabel={t('Close menu')} style={StyleSheet.absoluteFill} onPress={() => setPosition(undefined)} />
   {position && <View accessibilityViewIsModal style={[menu.surface, { top: position.y, left: position.x }]}>
    <Pressable accessibilityRole="button" onPress={() => choose(onFiles)} style={({ pressed }) => [menu.row, pressed && s.pressed]}><Folder size={20} color={ink} strokeWidth={1.7} /><Text style={menu.label}>{t('文件系统')}</Text></Pressable>
    <View style={menu.divider} />
    <Pressable accessibilityRole="button" onPress={() => choose(onPlugins)} style={({ pressed }) => [menu.row, pressed && s.pressed]}><Puzzle size={20} color={ink} strokeWidth={1.7} /><Text style={menu.label}>{t('Plugin market')}</Text></Pressable>
   </View>}
  </Modal>
 </View>
}
const menu = StyleSheet.create({
 surface: { position: 'absolute', width: 206, padding: 7, borderRadius: 19, borderWidth: 1, borderColor: edge, backgroundColor: '#fffdfdfa', boxShadow: '0 10px 36px rgba(33,42,66,0.13)' },
 row: { minHeight: 49, paddingHorizontal: 13, flexDirection: 'row', alignItems: 'center', gap: 13, borderRadius: 12 },
 label: { color: ink, fontSize: 15, fontWeight: '500' },
 divider: { height: StyleSheet.hairlineWidth, backgroundColor: edge, marginHorizontal: 12 },
})
