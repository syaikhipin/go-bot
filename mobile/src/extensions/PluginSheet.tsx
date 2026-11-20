import { useState } from 'react'
import { Alert } from 'react-native'
import type { TranscriptCache } from '../../../frontend/src/chat/transcriptCache'
import type { Controller } from '../api'
import { useLocale } from '../locale'
import { Sheet } from '../ui'
import { Plugins } from './Plugins'

export function PluginSheet({ api, cache, close }: { api: Controller; cache: TranscriptCache; close: () => void }) {
 const { t } = useLocale(), [dirty, setDirty] = useState(false)
 return <Sheet title={t('Plugin market')} onClose={close} beforeDismiss={proceed => {
  if (!dirty) { proceed(); return }
  Alert.alert(t('放弃未保存的修改？'), undefined, [{ text: t('取消'), style: 'cancel' }, { text: t('放弃修改'), style: 'destructive', onPress: proceed }])
 }}><Plugins api={api} cache={cache} onDirtyChange={setDirty} onBusyChange={ignoreBusy} /></Sheet>
}

function ignoreBusy(_busy: boolean) {}
