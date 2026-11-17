import { useEffect, useState, type FormEvent } from 'react'
import { useLocale } from '../locale'
import { Plus } from 'lucide-react'
import { APIError, deleteEndpoint, getModelSettings, saveDefaultEndpoint, saveEndpoint, settingsApplyFailed, type ModelEndpoint, type ModelSettings } from '../api/controller'

export function Models({ userId }: { userId: string }) {
  const { t } = useLocale()
  const [settings, setSettings] = useState<ModelSettings>()
  const [editing, setEditing] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  useEffect(() => {
    const request = new AbortController()
    void getModelSettings(userId, request.signal).then(setSettings, () => {
      if (!request.signal.aborted) setError('Could not load model settings.')
    })
    return () => request.abort()
  }, [userId])

  async function update(action: () => Promise<unknown>, notice: string) {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await action()
      setSettings(await getModelSettings(userId))
      setEditing(undefined)
      setNotice(notice)
    } catch (error) {
      if (settingsApplyFailed(error)) {
        const saved = await getModelSettings(userId).catch(() => undefined)
        if (saved) { setSettings(saved); setEditing(undefined) }
        setError('Settings were saved, but the Host could not apply them. Reopen the account to load the saved settings.')
      } else if (error instanceof APIError && error.message === 'api_key_required_for_new_origin') {
        setError('Enter an API key for the new endpoint address.')
      } else if (error instanceof APIError && error.message === 'invalid_model_endpoint') {
        setError('Check the endpoint URL, API key, and model IDs. Model IDs must be unique.')
      } else {
        setError('Could not confirm the change. Refresh settings before trying again.')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <h2 className="settings-section-title">{t('Models')}</h2>
      <p className="model-muted">{t('Only OpenAI-compatible Responses API endpoints are supported.')}</p>
      {error && <p role="alert" className="account-error">{t(error)}</p>}
      {notice && <p role="status">{t(notice)}</p>}
      {!settings && !error && <p role="status">{t('Loading model settings…')}</p>}
      {settings && <>
        {!Object.keys(settings.endpoints).length && <p className="model-muted">{t('Add an endpoint to create your first chat.')}</p>}
        {Object.entries(settings.endpoints).map(([id, endpoint]) =>
          <section className="model-card" key={id}>
            {editing === id ? <EndpointForm endpoint={endpoint} busy={busy} onCancel={() => setEditing(undefined)}
              onSave={value => { void update(() => saveEndpoint(userId, id, value), 'Endpoint saved.') }} /> : <>
              <h2>{endpoint.name} {settings.defaultEndpointId === id && <span className="model-default">{t('Default endpoint')}</span>}</h2>
              <p className="model-muted">{endpoint.baseUrl} · {endpoint.hasApiKey ? t('API key saved') : t('API key missing')}</p>
              <ul className="model-list">{endpoint.models.map((model, index) => <li key={model}>{model}{index === 0 && <span> · {t('Default model')}</span>}</li>)}</ul>
              <div className="model-settings-actions">
                <button type="button" disabled={busy} className="model-secondary" onClick={() => setEditing(id)}>{t('Edit')}</button>
                {settings.defaultEndpointId !== id && <button type="button" disabled={busy} className="model-secondary"
                  onClick={() => { void update(() => saveDefaultEndpoint(userId, id), 'Default endpoint updated.') }}>{t('Set as default')}</button>}
                <button type="button" disabled={busy} className="model-secondary" onClick={() => {
                  if (window.confirm(t('Remove this endpoint? Chats that use it will need a new model.'))) {
                    void update(() => deleteEndpoint(userId, id), 'Endpoint removed.')
                  }
                }}>{t('Remove')}</button>
              </div>
            </>}
          </section>
        )}
        {editing === '' ? <section className="model-card"><EndpointForm busy={busy} onCancel={() => setEditing(undefined)}
          onSave={value => { void update(() => saveEndpoint(userId, undefined, value), 'Endpoint added.') }} /></section> :
          <button className="model-secondary settings-add" type="button" disabled={busy} onClick={() => setEditing('')}><Plus size={17} /> {t('Add endpoint')}</button>}
      </>}
    </>
  )
}

type EndpointInput = Omit<ModelEndpoint, 'hasApiKey'> & { apiKey?: string }

function EndpointForm({ endpoint, busy, onSave, onCancel }: {
  endpoint?: ModelEndpoint
  busy: boolean
  onSave: (input: EndpointInput) => void
  onCancel: () => void
}) {
  const { t } = useLocale()
  const [name, setName] = useState(endpoint?.name ?? '')
  const [baseUrl, setBaseURL] = useState(endpoint?.baseUrl ?? '')
  const [key, setKey] = useState('')
  const [models, setModels] = useState(endpoint?.models.join('\n') ?? '')

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    onSave({ name: name.trim(), baseUrl: baseUrl.trim(), models: models.split('\n').map(model => model.trim()).filter(Boolean),
      ...(key.trim() ? { apiKey: key.trim() } : {}) })
  }

  return <form className="model-form" onSubmit={submit}>
    <h2>{endpoint ? t('Edit endpoint') : t('Add endpoint')}</h2>
    <fieldset disabled={busy}>
      <label>{t('Name')}<input value={name} required maxLength={128} onChange={event => setName(event.target.value)} /></label>
      <label>{t('Base URL')}<input type="url" value={baseUrl} required placeholder="https://api.example.com/v1" onChange={event => setBaseURL(event.target.value)} /></label>
      <label>{t('API key')}<input type="password" value={key} required={!endpoint?.hasApiKey} autoComplete="new-password"
        placeholder={endpoint?.hasApiKey ? t('Leave blank to keep saved key') : undefined} onChange={event => setKey(event.target.value)} /></label>
      <label>{t('Model IDs')}<textarea rows={4} value={models} required onChange={event => setModels(event.target.value)}
        aria-describedby="model-order" /><small id="model-order">{t('One model per line. The first model is the endpoint default.')}</small></label>
      <div className="model-settings-actions">
        <button type="submit" className="account-submit">{busy ? t('Saving…') : t('Save endpoint')}</button>
        <button type="button" className="model-secondary" onClick={onCancel}>{t('Cancel')}</button>
      </div>
    </fieldset>
  </form>
}
