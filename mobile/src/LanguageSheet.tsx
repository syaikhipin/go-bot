import { useState } from 'react'
import { Pressable, Text } from 'react-native'
import { Check } from 'lucide-react-native'
import { languages, useLocale, type Language } from './locale'
import { accent, glass, Hint, s, Sheet, SheetScroll, wash } from './ui'

export function LanguageSheet({ onClose }: { onClose: () => void }) {
  const { t, language, changeLanguage } = useLocale()
  const [saveFailed, setSaveFailed] = useState(false)

  async function select(next: Language) {
    setSaveFailed(false)
    try {
      await changeLanguage(next)
    } catch {
      setSaveFailed(true)
    }
  }

  return <Sheet title={t('语言')} onClose={onClose} raised={62}><SheetScroll>
    {languages.map(item => {
      const selected = item.id === language
      return <Pressable key={item.id} accessibilityRole="radio" accessibilityState={{ checked: selected }}
        style={[s.row, { minHeight: 49, paddingHorizontal: 14, borderRadius: 14, backgroundColor: selected ? wash : glass }]}
        onPress={() => void select(item.id)}>
        <Text style={[s.body, { flex: 1 }]}>{item.name}</Text>
        {selected && <Check size={18} color={accent} />}
      </Pressable>
    })}
    {saveFailed && <Hint error>{t('保存语言失败，请重试。')}</Hint>}
  </SheetScroll></Sheet>
}
