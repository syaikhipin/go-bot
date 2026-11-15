import { Check, ChevronDown, ExternalLink } from 'lucide-react'
import { useEffect, useState } from 'react'
import { APIError, getProviderSettings, removeProviderKey, saveProviderSettings, settingsApplyFailed, type Provider, type ProviderPage, type ProviderSettings } from '../api/controller'

import providerDocs from '../../../docs/providers.md?url&no-inline'

const keyHelp: Record<string, { label: string; url: string }> = {
  deepseek: { label: 'DeepSeek Search API', url: 'https://platform.deepseek.com/' },
  zhipu: { label: 'BigModel API key', url: 'https://bigmodel.cn/usercenter/proj-mgmt/apikeys' },
  doubao: { label: 'Doubao Search Custom API key', url: 'https://docs.volcengine.com/docs/networked-search/networked-search?lang=zh' },
  seedream: { label: 'Volcengine Ark API key', url: 'https://console.volcengine.com/ark/' },
  openai: { label: 'OpenAI API key', url: 'https://platform.openai.com/api-keys' },
  nanobanana: { label: 'Google AI Studio API key', url: 'https://aistudio.google.com/app/apikey' },
}

type Action = 'save' | 'activate' | 'remove'

export function ToolProviders({ userId, page }: { userId: string; page: ProviderPage }) {
  const isImage = page === 'image-generation'
  const title = isImage ? 'Image generation' : 'Web search'
  const [settings, setSettings] = useState<ProviderSettings>()
  const [editing, setEditing] = useState<string>()
  const [model, setModel] = useState('')
  const [apiKey, setAPIKey] = useState('')
  const [action, setAction] = useState<Action>()
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [loadAttempt, setLoadAttempt] = useState(0)
  const [retry, setRetry] = useState<{ id: string; action: 'activate' | 'remove' }>()

  function edit(provider: Provider, next: ProviderSettings) {
    setEditing(provider.id)
    setModel(next.entries[provider.id]?.model ?? provider.defaultModel ?? '')
    setAPIKey('')
  }

  useEffect(() => {
    const request = new AbortController()
    void getProviderSettings(userId, page, request.signal).then(next => {
      if (!request.signal.aborted) setSettings(next)
    }, () => {
      if (!request.signal.aborted) setError('Could not load provider settings.')
    })
    return () => request.abort()
  }, [userId, page, loadAttempt])

  async function update(provider: Provider, nextAction: Action) {
    setAction(nextAction)
    setError('')
    setNotice('')
    try {
      const next = nextAction === 'remove'
        ? await removeProviderKey(userId, page, provider.id)
        : await saveProviderSettings(userId, page, nextAction === 'activate'
          ? { provider: provider.id, activate: true }
          : { provider: provider.id, activate: false, model: isImage ? model.trim() : undefined, apiKey: apiKey.trim() || undefined })
      setSettings(next)
      if (editing === provider.id) edit(provider, next)
      setRetry(undefined)
      setNotice(nextAction === 'activate' ? `${provider.name} is now in use.`
        : nextAction === 'remove' ? `${provider.name} API key removed.` : `${provider.name} settings saved.`)
    } catch (failure) {
      if (settingsApplyFailed(failure)) {
        const saved = await getProviderSettings(userId, page).catch(() => undefined)
        if (saved) {
          setSettings(saved)
          if (editing === provider.id) edit(provider, saved)
        }
        setRetry({ id: provider.id, action: nextAction === 'remove' ? 'remove' : 'activate' })
        setError('Settings were saved, but the Host could not apply them. Retry or reopen the account.')
      } else {
        const code = failure instanceof APIError ? failure.message : ''
        setError(code === 'invalid_image_model' ? 'Enter a valid model name or endpoint ID.'
          : code === 'provider_key_required' || code === 'invalid_web_search_key' ? 'Save an API key before using this provider.'
          : code === 'invalid_provider_key' ? 'Enter a valid API key.'
          : 'Could not confirm the change. Try again.')
      }
    } finally {
      setAction(undefined)
    }
  }

  const active = settings?.providers.find(provider => provider.id === settings.provider)
  const activeReady = active && settings?.entries[active.id]?.hasApiKey

  return <div className="provider-settings">
    <div className="provider-settings-intro">
      <h2 className="settings-section-title">{title}</h2>
      <p className="model-muted">{isImage
        ? 'Choose the service your agent uses to generate images.'
        : 'Choose the service your agent uses to search the web.'}</p>
    </div>
    {error && <p className="account-error" role="alert">{error}</p>}
    {notice && <p className="provider-notice" role="status">{notice}</p>}
    {!settings && (error
      ? <button type="button" className="model-secondary" onClick={() => { setError(''); setLoadAttempt(attempt => attempt + 1) }}>Retry</button>
      : <p role="status">Loading provider settings…</p>)}
    {settings && <>
      <p className="provider-current">
        {retry ? <>Waiting for the Host to apply the saved change.</> : activeReady ? <>In use <strong>{active.name}</strong></> : <>No provider is in use. Save a key and choose a provider.</>}
      </p>
      <ul className="provider-list" aria-label={`${title} providers`}>
        {settings.providers.map(provider => {
          const entry = settings.entries[provider.id]
          const hasKey = !!entry?.hasApiKey
          const selected = provider.id === settings.provider
          const expanded = editing === provider.id
          const help = keyHelp[provider.id]
          const dirty = !!apiKey.trim() || (isImage && model.trim() !== (entry?.model ?? provider.defaultModel))
          const pending = retry?.id === provider.id ? retry : undefined
          const formId = `provider-${page}-${provider.id}`
          return <li className="provider-row" key={provider.id}>
            <div className="provider-row-main">
              <div className="provider-row-identity">
                <h3>{provider.name}</h3>
                <p>{isImage && hasKey ? entry?.model ?? provider.defaultModel : hasKey ? 'API key saved' : 'No API key'}</p>
              </div>
              <div className="provider-row-actions">
                {selected && hasKey && !pending && <span className="provider-active"><Check size={15} aria-hidden="true" /> In use</span>}
                {(pending || (hasKey && !selected)) && <button type="button" className="model-secondary provider-use"
                  aria-label={`${pending ? 'Retry applying' : 'Use'} ${provider.name}`}
                  disabled={!!action || (expanded && dirty)} onClick={() => { void update(provider, pending?.action ?? 'activate') }}>
                  {pending ? 'Retry apply' : 'Use provider'}
                </button>}
                <button type="button" className="provider-edit" aria-label={`${hasKey ? 'Edit' : 'Set up'} ${provider.name}`}
                  aria-expanded={expanded} aria-controls={expanded ? formId : undefined} disabled={!!action}
                  onClick={() => {
                    setNotice('')
                    if (!retry) setError('')
                    if (expanded) { setEditing(undefined); setAPIKey('') }
                    else edit(provider, settings)
                  }}>
                  {hasKey ? 'Edit' : 'Set up'} <ChevronDown size={15} aria-hidden="true" />
                </button>
              </div>
            </div>
            {expanded && <form id={formId} className="model-form provider-form" onSubmit={event => {
              event.preventDefault()
              void update(provider, 'save')
            }}>
              <fieldset disabled={!!action}>
                {isImage && <label>{provider.id === 'seedream' ? 'Model name or endpoint ID' : 'Model name'}
                  <input value={model} required maxLength={256} onChange={event => setModel(event.target.value)} />
                </label>}
                <label>API key
                  <input type="password" value={apiKey} required={!hasKey} maxLength={4096} autoComplete="new-password"
                    placeholder={hasKey ? 'Leave blank to keep the saved key' : undefined}
                    onChange={event => setAPIKey(event.target.value)} />
                </label>
                {help && <a className="provider-doc-link" href={help.url} target="_blank" rel="noreferrer">{help.label}<ExternalLink size={13} aria-hidden="true" /></a>}
                <div className="model-settings-actions">
                  <button type="submit" className="account-submit" disabled={!dirty || (!hasKey && !apiKey.trim())}>
                    {action === 'save' ? 'Saving…' : 'Save settings'}
                  </button>
                  {hasKey && <button type="button" className="model-secondary" onClick={() => { void update(provider, 'remove') }}>Remove key</button>}
                </div>
              </fieldset>
            </form>}
          </li>
        })}
      </ul>
      <a className="provider-doc-link" href={providerDocs} target="_blank" rel="noreferrer">Build a provider adapter <ExternalLink size={13} aria-hidden="true" /></a>
    </>}
  </div>
}
