import { StyleSheet, View } from 'react-native'
import { FileArchive, FileAudio, FileCode, FileImage, FileText, FileVideo } from 'lucide-react-native'
import { SvgXml } from 'react-native-svg'
import icons from '../assets/file-icons/vscode-icons.json'

const office = {
  pdf: { svg: icons.pdf, background: '#fbe5e8' },
  word: { svg: icons.word, background: '#e5edff' },
  excel: { svg: icons.excel, background: '#dff2e7' },
  powerpoint: { svg: icons.powerpoint, background: '#ffecdf' },
  markdown: { svg: icons.markdown, background: '#e9eafa' },
}

const formats: Record<string, keyof typeof office> = {
  pdf: 'pdf', doc: 'word', docx: 'word', docm: 'word', odt: 'word', rtf: 'word',
  xls: 'excel', xlsx: 'excel', xlsm: 'excel', ods: 'excel', csv: 'excel', tsv: 'excel',
  ppt: 'powerpoint', pptx: 'powerpoint', pptm: 'powerpoint', odp: 'powerpoint',
  md: 'markdown', markdown: 'markdown', mdx: 'markdown',
}

export function FileIcon({ name, size = 38 }: { name: string; size?: number }) {
  const extension = name.includes('.') ? name.split('.').at(-1)?.toLowerCase() ?? '' : ''
  const branded = office[formats[extension]]
  const [Icon, background, color] =
    /^(zip|tar|gz|rar|7z)$/.test(extension) ? [FileArchive, '#f8ecd8', '#98611a'] :
    /^(png|jpeg|jpg|heic|gif|webp|svg)$/.test(extension) ? [FileImage, '#dff2eb', '#368365'] :
    /^(mp3|wav|m4a|flac)$/.test(extension) ? [FileAudio, '#eee7fc', '#70509e'] :
    /^(mp4|mov|mkv|webm)$/.test(extension) ? [FileVideo, '#eee7fc', '#70509e'] :
    /^(js|ts|json|xml|html|css|py|go|sh|yaml|yml)$/.test(extension) ? [FileCode, '#e4edfc', '#41678c'] :
    [FileText, '#e9edf7', '#52627f']
  return <View style={[styles.tile, { width: size, height: size, borderRadius: size * 0.28, backgroundColor: branded?.background ?? background }]}>
    {branded ? <SvgXml xml={branded.svg} width={size * 0.65} height={size * 0.65} />
      : <Icon size={size * 0.54} color={color} strokeWidth={1.7} />}
  </View>
}

const styles = StyleSheet.create({ tile: { alignItems: 'center', justifyContent: 'center', flexShrink: 0 } })
