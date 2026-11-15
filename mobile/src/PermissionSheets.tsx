import { useState } from 'react'
import { Pressable, Text, View } from 'react-native'
import { Check, ShieldAlert } from 'lucide-react-native'
import { permissionOptions, type PermissionMode } from '../../frontend/src/chat/permissions'
import type { PendingQuestion } from '../../frontend/src/chat/transcript'
import { useLocale } from './locale'
import { accent, Button, glass, Hint, s, Sheet, SheetScroll, TintIcon, wash } from './ui'

export const permissionNames: Record<PermissionMode, string> = {
  'danger-full-access': '全权限', 'workspace-write': '工作区可写', 'read-only': '只读',
}

const descriptions: Record<PermissionMode, string> = {
  'danger-full-access': '允许工具在 Host 可访问的范围内写入。',
  'workspace-write': '允许写入当前会话工作区。超出范围时需要审批。',
  'read-only': '允许读取文件。写入前需要审批。',
}

export function PermissionSheet({ mode, close, choose }: {
  mode: PermissionMode; close: () => void; choose: (mode: PermissionMode) => Promise<unknown>
}) {
  const { t } = useLocale()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function select(next: PermissionMode) {
    if (next === mode) { close(); return }
    setBusy(true)
    setError('')
    try { await choose(next); close() }
    catch (e) { setError(e instanceof Error ? e.message : '无法切换会话权限。') }
    finally { setBusy(false) }
  }
  return <Sheet title={t('当前会话权限')} onClose={close}><SheetScroll>
    {permissionOptions.map(({ mode: option }) => <Pressable key={option} accessibilityRole="button" accessibilityState={{ selected: mode === option }}
      disabled={busy} onPress={() => void select(option)}
      style={[s.row, { borderRadius: 14, padding: 13, minHeight: 59, backgroundColor: mode === option ? wash : glass }]}>
      <View style={{ flex: 1, gap: 4 }}>
        <Text style={[s.body, { fontWeight: '600' }]}>{t(permissionNames[option])}</Text>
        <Hint>{t(descriptions[option])}</Hint>
      </View>
      {mode === option && <Check size={18} color={accent} />}
    </Pressable>)}
    {!!error && <Hint error>{t(error)}</Hint>}
  </SheetScroll></Sheet>
}

export function ApprovalSheet({ item, close, decide }: {
  item: PendingQuestion; close: () => void; decide: (decision: 'Allow once' | 'Deny') => Promise<unknown>
}) {
  const { t } = useLocale()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const info = item.approval
  if (!info) return null
  async function submit(decision: 'Allow once' | 'Deny') {
    if (busy) return
    setBusy(true)
    setError('')
    try { await decide(decision); close() }
    catch { setError('无法提交，请重试。'); setBusy(false) }
  }
  return <Sheet title={t('需要执行审批')} onClose={close} raised={64}><SheetScroll>
    <View style={[s.row, { gap: 11 }]}>
      <TintIcon background={wash}><ShieldAlert color={accent} size={20} /></TintIcon>
      <View style={{ flex: 1 }}><Text style={s.body}>{info.tool}</Text>
      {info.target && <Text style={s.caption}>{t(permissionNames[info.target])}</Text>}</View>
    </View>
    <Text style={s.body}>{info.reason}</Text>
    {!!info.detail && <View style={{ padding: 12, backgroundColor: wash, borderRadius: 12 }}>
      <Text selectable style={[s.body, { fontFamily: 'monospace', fontSize: 13 }]}>{info.detail}</Text>
    </View>}
    <Hint>{t('仅允许这一次操作，会话权限保持不变。')}</Hint>
    {!!error && <Hint error>{t(error)}</Hint>}
    <View style={[s.row, { justifyContent: 'flex-end', gap: 8 }]}>
      <Button label={t('拒绝')} disabled={busy} onPress={() => void submit('Deny')} />
      <Button label={t('仅允许一次')} primary disabled={busy} onPress={() => void submit('Allow once')} />
    </View>
  </SheetScroll></Sheet>
}
