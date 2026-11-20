import { ProtocolDropdown } from './ProtocolDropdown'
import { imageGenerationSettingsEnabled } from '../../frontend/src/features'
import { changeProtocolURL, modelProtocol, modelRequestURL, protocolNames, protocolURLs, type ModelProtocol } from '../../frontend/src/settings/modelProtocol'
import { languages, useLocale } from './locale'
import { useEffect, useState } from 'react'
import { ActivityIndicator, Alert, Image, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native'
import { Camera, ChevronRight, Images, Image as Drawing, Languages, Layers2, LogOut, Power, Search, Server } from 'lucide-react-native'
import { Controller, problem, type ModelSettings, type ProviderPage, type ProviderSettings } from './api'
import { getAvatar, pickAvatar } from './library'
import { LanguageSheet } from './LanguageSheet'
import { accent, Button, Field, Hint, IconButton, edge, glass, ink, muted, Panel, s, Sheet, SheetScroll, wash } from './ui'

const providerLabels = { 'web-search': '网页搜索', 'image-generation': '图片生成' }
export type Section = 'models' | ProviderPage

export function Me({ api, section, open, close, exitAccount }: {
  api: Controller; section: Section | null; open: (section: Section) => void; close: () => void; exitAccount: () => void
}) {
  const { t, language } = useLocale()
  const { account } = api.profile
  const [avatar, setAvatar] = useState<string | undefined>(() => getAvatar(account))
  const [avatarMenu, setAvatarMenu] = useState(false)
  const [languageOpen, setLanguageOpen] = useState(false)
  const [serverOpen, setServerOpen] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [modelDirty, setModelDirty] = useState(false)
  const [error, setError] = useState('')
  async function changeAvatar(camera: boolean) {
    setError('')
    try {
      const uri = await pickAvatar(account, camera)
      if (uri) setAvatar(uri)
    } catch (issue) { setError(issue instanceof Error ? issue.message : '无法更新头像。') }
  }
  async function stop() {
    setStopping(true)
    setError('')
    try { await api.stopHost(); exitAccount() }
    catch (e) { setError(problem(e, '无法停止 Host，请重试。')); setStopping(false) }
  }

  return <>
    <ScrollView contentContainerStyle={{ paddingHorizontal: 20, paddingTop: 16, paddingBottom: 30, gap: 16, maxWidth: 620, width: '100%', alignSelf: 'center' }}>
      <Text accessibilityRole="header" style={s.title}>{t('我的')}</Text>
      <Pressable accessibilityRole="button" accessibilityLabel={t('更换头像')} onPress={() => setAvatarMenu(true)}
        style={[s.row, { gap: 16, minHeight: 74, marginTop: 2, marginBottom: 5, alignSelf: 'flex-start', maxWidth: '100%' }]}>
        <View style={{ width: 70, height: 70, borderRadius: 35, backgroundColor: '#dfd8f7', borderColor: '#fff', borderWidth: 1.5, alignItems: 'center', justifyContent: 'center' }}>
          {avatar ? <Image source={{ uri: avatar }} style={{ width: 67, height: 67, borderRadius: 34 }} /> : <Text style={{ fontSize: 28, fontWeight: '600', color: accent }}>{account.charAt(0).toUpperCase()}</Text>}
          <View style={{ position: 'absolute', right: -2, bottom: -2, backgroundColor: '#fff', borderRadius: 10, padding: 4 }}>
            <Camera size={12} color={accent} strokeWidth={1.7} />
          </View>
        </View>
        <Text numberOfLines={2} style={{ flexShrink: 1, fontSize: 23, fontWeight: '700', letterSpacing: -0.35, color: ink }}>{account}</Text>
      </Pressable>
      <Panel style={{ gap: 0, paddingVertical: 4 }}>
        {([
          ['models', '模型', Layers2],
          ['web-search', '网页搜索', Search],
          ['image-generation', '图片生成', Drawing],
          ['language', '语言', Languages],
        ] as const).filter(([id]) => id !== 'image-generation' || imageGenerationSettingsEnabled).map(([id, label, Icon], index) => <Pressable accessibilityRole="button" key={id}
          onPress={() => id === 'language' ? setLanguageOpen(true) : open(id)}
          style={[s.row, { minHeight: 54, gap: 14 }, index > 0 && { borderTopWidth: StyleSheet.hairlineWidth, borderColor: edge }]}>
          <Icon size={20} color={muted} strokeWidth={1.7} />
          <Text style={[s.body, { flex: 1 }]}>{t(label)}</Text>
          {id === 'language' && <Text style={s.caption}>{languages.find(item => item.id === language)?.name}</Text>}
          <ChevronRight size={15} color={muted} />
        </Pressable>)}
      </Panel>
      <Pressable accessibilityRole="button" accessibilityLabel={t('服务器地址')} onPress={() => setServerOpen(true)}>
        <Panel style={[s.row, { paddingVertical: 15, gap: 12 }]}><Server size={20} color={muted} strokeWidth={1.7} />
          <Text style={s.body}>{t('服务器地址')}</Text><Text numberOfLines={1} style={[s.caption, { flex: 1, textAlign: 'right' }]}>{api.profile.server}</Text>
          <ChevronRight size={15} color={muted} />
        </Panel>
      </Pressable>
      <Pressable accessibilityRole="button" disabled={stopping} onPress={exitAccount}>
        <Panel style={[s.row, { paddingVertical: 15, gap: 12 }]}>
          <LogOut size={20} color={muted} strokeWidth={1.7} />
          <Text style={s.body}>{t('退出登录')}</Text>
        </Panel>
      </Pressable>
      <Pressable accessibilityRole="button" disabled={stopping} onPress={() => Alert.alert(t('停止 Host？'), t('停止后会回到连接页。'), [
        { text: t('返回'), style: 'cancel' }, { text: 'Stop Host', style: 'destructive', onPress: stop },
      ])}>
        <Panel style={[s.row, { paddingVertical: 15, gap: 12 }]}>
          <Power size={20} color="#a04656" strokeWidth={1.7} />
          <Text style={[s.body, { color: '#a04656' }]}>Stop Host</Text>{stopping && <ActivityIndicator color={accent} />}
        </Panel>
      </Pressable>
      <Hint error>{t(error)}</Hint>
    </ScrollView>
    {languageOpen && <LanguageSheet onClose={() => setLanguageOpen(false)} />}
    {serverOpen && <Sheet title={t('服务器地址')} onClose={() => setServerOpen(false)} raised={62}><SheetScroll>
      <Panel><Text selectable style={s.body}>{api.profile.server}</Text></Panel>
    </SheetScroll></Sheet>}
    {avatarMenu && <Sheet title={t('更换头像')} onClose={() => setAvatarMenu(false)} raised={62}>{afterClose => <SheetScroll>
      <Button label={t('从图库选择')} onPress={() => afterClose(() => changeAvatar(false))}><Images size={19} color={accent} /></Button>
      <Button label={t('拍照')} onPress={() => afterClose(() => changeAvatar(true))}><Camera size={19} color={accent} /></Button>
    </SheetScroll>}</Sheet>}
    {section === 'models' && <Sheet title={t('模型')} onClose={() => { setModelDirty(false); close() }} beforeDismiss={proceed => {
      if (!modelDirty) { proceed(); return }
      Alert.alert(t('放弃未保存的修改？'), undefined, [{ text: t('取消'), style: 'cancel' }, { text: t('放弃修改'), style: 'destructive', onPress: proceed }])
    }}><Models api={api} onDirtyChange={setModelDirty} /></Sheet>}
    {section && section !== 'models' && (section !== 'image-generation' || imageGenerationSettingsEnabled) && <Sheet title={t(providerLabels[section])} onClose={close}><Providers key={section} api={api} page={section} /></Sheet>}

  </>
}

function Models({ api, onDirtyChange }: { api: Controller; onDirtyChange: (dirty: boolean) => void }) {
  const { t } = useLocale()
  const [settings, setSettings] = useState<ModelSettings>()
  const [editing, setEditing] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [baseUrl, setURL] = useState('')
  const [protocol, setProtocol] = useState<ModelProtocol>('responses')
  const [models, setModels] = useState('')
  const [apiKey, setKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  function load() { return api.modelSettings().then(setSettings).catch(e => setError(problem(e, '无法读取模型设置。'))) }
  useEffect(() => { void load() }, [api])

  function edit(id: string) {
    const endpoint = settings?.endpoints[id]
    setEditing(id)
    setName(endpoint?.name ?? '')
    setProtocol(modelProtocol(endpoint?.protocol))
    setURL(endpoint?.baseUrl ?? protocolURLs.responses)
    setModels(endpoint?.models.join('\n') ?? '')
    setKey('')
    setError('')
  }
  async function update(work: () => Promise<unknown>) {
    setBusy(true)
    setError('')
    try { await work(); setEditing(null); await load() }
    catch (e) { setError(problem(e, '无法保存。请检查端点地址、密钥与模型名。')); await load() }
    finally { setBusy(false) }
  }
  const list = models.split(/[\n,]/).map(model => model.trim()).filter(Boolean)
  useEffect(() => {
    const initial = editing === null ? undefined : settings?.endpoints[editing]
    onDirtyChange(editing !== null && (name !== (initial?.name ?? '') || baseUrl !== (initial?.baseUrl ?? protocolURLs.responses) || protocol !== modelProtocol(initial?.protocol) || apiKey !== '' || models !== (initial?.models.join('\n') ?? '')))
  }, [editing, settings, name, baseUrl, protocol, apiKey, models, onDirtyChange])


  return <SheetScroll>
    <Hint>{t('支持 OpenAI Responses 和 Anthropic Messages 兼容接口。默认端点的第一个模型用于新会话。')}</Hint>
    {Object.entries(settings?.endpoints ?? {}).map(([id, endpoint]) => <Pressable key={id} accessibilityRole="button" onPress={() => edit(id)}>
      <Panel style={{ backgroundColor: id === editing ? wash : glass }}>
        <View style={s.row}><Text style={[s.body, { flex: 1, fontWeight: '600' }]}>{endpoint.name}</Text>
          {settings?.defaultEndpointId === id && <Text style={s.caption}>{t('默认')}</Text>}<ChevronRight size={16} color={muted} /></View>
        <Hint>{protocolNames[modelProtocol(endpoint.protocol)]} · {endpoint.models.join(' · ')}</Hint>
      </Panel>
    </Pressable>)}
    {editing === null ? <Button label={t('添加端点')} onPress={() => edit('')} /> : <>
      <Field label={t('名称')} value={name} onChangeText={setName} />
      <ProtocolDropdown value={protocol} disabled={busy} onChange={next => { setURL(changeProtocolURL(baseUrl, protocol, next)); setProtocol(next) }} />
      <Hint>{t('按服务商提供的接口格式选择，与模型名称无关。')}</Hint>
      <Field label={t('端点地址')} keyboardType="url" autoCorrect={false} placeholder={protocolURLs[protocol]} value={baseUrl} onChangeText={setURL} />
      <Hint>{t('请求地址')}: {modelRequestURL(baseUrl, protocol)}</Hint>
      <Field label="API key" autoCorrect={false} secureTextEntry value={apiKey} onChangeText={setKey}
        placeholder={settings?.endpoints[editing]?.hasApiKey ? t('已保存，留空保留当前密钥') : t('输入 API key')} />
      <Field label={t('模型名（每行一个）')} autoCorrect={false} multiline value={models} onChangeText={setModels} />
      <Button primary label={t('保存端点')} busy={busy} disabled={!name.trim() || !baseUrl.trim() || !list.length || (!apiKey.trim() && !settings?.endpoints[editing]?.hasApiKey)}
        onPress={() => update(() => api.saveEndpoint(editing || undefined, { protocol, name: name.trim(), baseUrl: baseUrl.trim(), models: list, ...(apiKey.trim() && { apiKey: apiKey.trim() }) }))} />
      <Button label={t('取消')} disabled={busy} onPress={() => setEditing(null)} />
      {!!editing && settings?.defaultEndpointId !== editing && <Button label={t('设为默认端点')} disabled={busy} onPress={() => update(() => api.setDefaultEndpoint(editing))} />}
      {!!editing && <Button label={t('删除端点')} dangerTone disabled={busy} onPress={() => Alert.alert(t('删除端点？'), name, [{ text: t('返回'), style: 'cancel' }, { text: t('删除'), style: 'destructive', onPress: () => update(() => api.removeEndpoint(editing)) }])} />}
    </>}
    <Hint error>{t(error)}</Hint>
  </SheetScroll>
}

function Providers({ api, page }: { api: Controller; page: ProviderPage }) {
  const { t } = useLocale()
  const [data, setData] = useState<ProviderSettings>()
  const [selected, setSelected] = useState('')
  const [key, setKey] = useState('')
  const [model, setModel] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  function select(id: string, settings: ProviderSettings) {
    setSelected(id)
    setKey('')
    setModel(settings.entries[id]?.model ?? settings.providers.find(p => p.id === id)?.defaultModel ?? '')
  }
  useEffect(() => {
    api.providers(page).then(settings => {
      setData(settings)
      select(settings.provider || settings.defaultProvider || settings.providers[0]?.id || '', settings)
    }).catch(e => setError(problem(e, '无法读取供应商。')))
  }, [api, page])

  async function update(work: () => Promise<ProviderSettings>) {
    setBusy(true)
    setError('')
    try { const settings = await work(); setData(settings); select(selected, settings) }
    catch (e) {
      setError(problem(e, '无法保存供应商设置。'))
      api.providers(page).then(setData).catch(() => {})
    } finally { setBusy(false) }
  }
  const entry = data?.entries[selected]
  return <SheetScroll>
    {data?.providers.map(provider => <Pressable accessibilityRole="button" accessibilityState={{ selected: provider.id === selected }} key={provider.id} onPress={() => select(provider.id, data)}>
      <View style={[s.row, { minHeight: 48, paddingHorizontal: 12, borderRadius: 15, backgroundColor: provider.id === selected ? wash : glass }]}>
        <Text style={[s.body, { flex: 1 }]}>{provider.name}</Text>
        <Text style={s.caption}>{data.provider === provider.id ? t('当前使用') : data.entries[provider.id]?.hasApiKey ? t('已配置') : ''}</Text>
      </View>
    </Pressable>)}
    {!!selected && <>
      <Field label="API key" secureTextEntry autoCorrect={false} value={key} onChangeText={setKey} placeholder={entry?.hasApiKey ? t('已保存，留空保留当前密钥') : t('输入 API key')} />
      {page === 'image-generation' && <Field label={t('模型名')} value={model} onChangeText={setModel} autoCorrect={false} />}
      <Button label={t('保存配置')} disabled={!key.trim() && !entry?.hasApiKey} busy={busy} onPress={() => update(() => api.saveProvider(page, {
        provider: selected, ...(key.trim() && { apiKey: key.trim() }), ...(page === 'image-generation' && model.trim() && { model: model.trim() }), activate: false,
      }))} />
      <Button primary label={t('保存并使用')} disabled={!key.trim() && !entry?.hasApiKey} busy={busy} onPress={() => update(() => api.saveProvider(page, {
        provider: selected, ...(key.trim() && { apiKey: key.trim() }), ...(page === 'image-generation' && model.trim() && { model: model.trim() }), activate: true,
      }))} />
      {entry?.hasApiKey && <Button label={t('移除密钥')} dangerTone disabled={busy} onPress={() => update(() => api.removeProvider(page, selected))} />}
    </>}
    <Hint error>{t(error)}</Hint>
  </SheetScroll>
}
