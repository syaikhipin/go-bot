import { useLocale } from './locale'
import { useState } from 'react'
import { ActivityIndicator, Image, Linking, Pressable, StyleSheet, Text, View } from 'react-native'
import Markdown from 'react-native-markdown-display'
import { ChevronDown, CircleCheck, CircleX, Download, ImageOff, Layers2 } from 'lucide-react-native'
import { groupDisplayBlocks, type Message as Item, type ToolBlock, type Transcript } from '../../frontend/src/chat/transcript'
import { Controller } from './api'
import { FileIcon } from './FileIcon'
import { saveRemote, shareDownload, shareLibraryItem, type RemoteFile, type WorkspaceFileSource } from './library'
import { accent, edge, glass, Hint, ink, line, logo, muted, s, wash } from './ui'

export type Preview = RemoteFile & { width?: number; height?: number }
function formatted(text: string) {
  return <Markdown style={markdown} rules={{
    image: node => <Text key={node.key} selectable>{'![' + (node.attributes.alt ?? '') + '](' + node.attributes.src + ')'}</Text>,
  }} onLinkPress={url => {
    if (/^https?:\/\//i.test(url)) void Linking.openURL(url).catch(() => {})
    return false
  }}>{text}</Markdown>
}

export function MessageView({ api, id, message, results, openImage, generating = false }: { api: Controller; id: string; message: Item; results: Transcript['toolResults']; openImage: (image: Preview) => void; generating?: boolean }) {
  const { t } = useLocale()
  if (message.role === 'notice') return <View style={styles.notice}><Hint>{message.blocks.map(block => block.type === 'text' ? t(block.text) : '').join(' ')}</Hint></View>
  const user = message.role === 'user'
  return <View style={[styles.message, user && { alignItems: 'flex-end' }]}>
    {!user && <View style={[s.row, { minHeight: 34 }]}>
      <Image source={logo} style={styles.avatar} accessibilityLabel="Go Bot" />
      {generating && <><ActivityIndicator size="small" color={accent} /><Text accessibilityLiveRegion="polite" style={s.caption}>{t('生成中')}</Text></>}
    </View>}
    <View style={[styles.content, user && styles.bubble]}>
      {groupDisplayBlocks(message.blocks, results).map(block => {
        if (block.type === 'text') return <View key={block.id}>{formatted(block.text)}</View>
        if (block.type === 'file') {
          const delivery = block.file.source === 'presented'
            ? { account: api.profile.account, path: block.file.path }
            : undefined
          return <FileTile key={block.id} delivery={delivery} preview={{ uri: api.fileURL(id, block.file, block.name), headers: api.headers, name: block.name || t('附件') }} />
        }
        if (block.type === 'tool') return <Tools key={block.id} tools={[block]} results={results} />
        if (block.type === 'tool-group') return <Tools key={block.id} tools={block.tools} results={results} />
        return <View key={block.id} style={styles.images}>
          {block.images.map(({ id: key, attachment }) => {
            const preview = { uri: api.fileURL(id, { source: 'attachment', attachmentId: attachment.attachmentId }),
              headers: api.headers, name: attachment.name || t('图片'), mimeType: attachment.mediaType, width: attachment.width, height: attachment.height }
            return <Thumb key={key} preview={preview} open={() => openImage(preview)} />
          })}
        </View>
      })}
    </View>
  </View>
}

function Thumb({ preview, open }: { preview: Preview; open: () => void }) {
  const [failed, setFailed] = useState(false)
  return <Pressable accessibilityRole="imagebutton" accessibilityLabel={preview.name} onPress={open} style={styles.thumb}>
    {failed ? <ImageOff color={muted} size={21} /> : <Image source={preview} onError={() => setFailed(true)} style={{ width: '100%', height: '100%' }} resizeMode="cover" />}
  </Pressable>
}

function FileTile({ preview, delivery }: { preview: Preview; delivery?: WorkspaceFileSource }) {
  const { t } = useLocale()
  const [busy, setBusy] = useState(false)
  const [failed, setFailed] = useState(false)
  return <View>
    <Pressable accessibilityRole="button" accessibilityLabel={t('下载') + ' ' + preview.name} disabled={busy} style={styles.tile} onPress={async () => {
      setBusy(true)
      setFailed(false)
      try {
        if (delivery) {
          const saved = await saveRemote(preview, 'file', delivery)
          await shareLibraryItem(saved)
        } else await shareDownload(preview.uri, preview.headers, preview.name)
      }
      catch { setFailed(true) }
      finally { setBusy(false) }
    }}>
      <FileIcon name={preview.name} />
      <View style={{ flex: 1, gap: 2 }}>
        <Text style={{ color: ink, fontSize: 15, fontWeight: '600' }} numberOfLines={2}>{preview.name}</Text>
        <Text style={{ color: muted, fontSize: 12 }}>{t(delivery ? '已交付' : '附件')}</Text>
      </View>
      {busy ? <ActivityIndicator color={accent} /> : <Download size={17} color={muted} />}
    </Pressable>
    {failed && <Hint error>{t('文件暂不可用。')}</Hint>}
  </View>
}

function Tools({ tools, results }: { tools: ToolBlock[]; results: Transcript['toolResults'] }) {
  const { t, language } = useLocale()
  const [expanded, setExpanded] = useState(false)
  return <View style={styles.tools}>
    <Pressable accessibilityRole="button" accessibilityState={{ expanded }} style={styles.toolHeader} onPress={() => setExpanded(old => !old)}>
      <Layers2 color="#7864b7" size={19} strokeWidth={1.8} />
      <Text numberOfLines={1} style={[s.caption, { flex: 1 }]}>{tools.length > 1 ? tools.length + (language === 'en' ? ' ' : ' 个') + t('工具调用') : tools[0].name}</Text>
      <ChevronDown color={muted} size={16} style={{ transform: [{ rotate: expanded ? '180deg' : '0deg' }] }} />
    </Pressable>
    {expanded && tools.map(tool => {
      const result = results[tool.callId]
      return <View key={tool.id} style={styles.toolRow}>
        {result?.status === 'succeeded' ? <CircleCheck size={15} color={accent} /> : result?.status ? <CircleX size={15} color="#9d5e5b" /> : <ActivityIndicator size="small" color={accent} />}
        <Text selectable style={[s.caption, { flex: 1 }]}>{tool.name}{result?.code ? ' · ' + result.code : ''}</Text>
      </View>
    })}
  </View>
}

const styles = StyleSheet.create({
  notice: { alignItems: 'center', paddingVertical: 8 },
  message: { gap: 4 },
  avatar: { width: 36, height: 36 },
  content: { flexShrink: 1, minWidth: 0, gap: 8 },
  bubble: { maxWidth: '83%', borderRadius: 20, backgroundColor: '#fff', borderColor: edge, borderWidth: StyleSheet.hairlineWidth, paddingHorizontal: 14, paddingVertical: 6, boxShadow: '0 3px 12px rgba(45,35,70,0.055)' },
  images: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  thumb: { width: '48%', maxWidth: 160, aspectRatio: 1, overflow: 'hidden', borderRadius: 15, borderColor: '#fff', borderWidth: 1.5, backgroundColor: '#e8ecfa', alignItems: 'center', justifyContent: 'center' },
  tile: { minHeight: 60, flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 11, paddingVertical: 8, borderRadius: 15, backgroundColor: glass, borderColor: edge, borderWidth: 1 },
  tools: { alignSelf: 'stretch', backgroundColor: glass, borderRadius: 14, borderColor: edge, borderWidth: 1, overflow: 'hidden' },
  toolHeader: { minHeight: 46, flexDirection: 'row', alignItems: 'center', paddingHorizontal: 12, gap: 8 },
  toolRow: { minHeight: 38, paddingHorizontal: 12, flexDirection: 'row', alignItems: 'center', gap: 8, borderTopColor: edge, borderTopWidth: StyleSheet.hairlineWidth },
})

const markdown = StyleSheet.create({
  body: { color: ink, fontSize: 16, lineHeight: 25 },
  paragraph: { marginTop: 4, marginBottom: 5 },
  heading1: { fontSize: 24, lineHeight: 30, fontWeight: '700', marginVertical: 4 },
  heading2: { fontSize: 21, lineHeight: 27, fontWeight: '700', marginVertical: 4 },
  heading3: { fontSize: 18, lineHeight: 24, fontWeight: '600' },
  table: { borderColor: line, borderWidth: 1, borderRadius: 6 },
  tr: { borderBottomColor: line },
  th: { backgroundColor: wash, padding: 5 },
  td: { minWidth: 0, padding: 5 },
  link: { color: accent, textDecorationLine: 'underline' },
  code_inline: { backgroundColor: '#eff1ed', color: ink, fontSize: 13 },
  code_block: { backgroundColor: '#eff1ed', borderColor: line, color: ink, fontSize: 13 },
  fence: { backgroundColor: '#eff1ed', borderColor: line, color: ink, fontSize: 13 },
  blockquote: { backgroundColor: '#f6f7f3', borderColor: '#c3d3ca' },
})
