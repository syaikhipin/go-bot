import { useCallback, useEffect, useRef, useState } from 'react'
import { LanguagePicker, useLocale } from './locale'
import { ArrowLeft, Globe, Image, Menu, SlidersHorizontal } from 'lucide-react'
import { APIError, archiveSession, createSession, listSessions, openAccount, readHostStream, stopHost, type ProviderPage, type SessionRow } from './api/controller'
import { AccountEntry } from './AccountEntry'
import { Chat } from './chat/Chat'
import { TranscriptCache } from './chat/transcriptCache'
import { HostConnection } from './chat/hostConnection'
import { Sidebar } from './sessions/Sidebar'
import { Models } from './settings/Models'
import { ToolProviders } from './settings/ToolProviders'
import { HostFiles } from './files/HostFiles'
import { Plugins } from './extensions/Plugins'
import { ExtensionDialogs } from './extensions/Dialogs'
import { imageGenerationSettingsEnabled } from './features'

type SettingsPage = 'models' | ProviderPage | 'plugins'

function currentSettings(): SettingsPage | undefined {
  if (window.location.pathname === '/settings/models') return 'models'
  if (window.location.pathname === '/settings/web-search') return 'web-search'
  if (window.location.pathname === '/settings/image-generation') return imageGenerationSettingsEnabled ? 'image-generation' : 'models'
  if (window.location.pathname === '/plugins') return 'plugins'
}

function settingsNavigationAllowed(resume: () => void) {
  return window.dispatchEvent(new CustomEvent('go-bot:settings-navigation', { cancelable: true, detail: { resume } }))
}

function currentSession() {
  return /^\/chat\/([\da-f]{8}-(?:[\da-f]{4}-){3}[\da-f]{12})$/i.exec(window.location.pathname)?.[1]
}

function errorDeleting(error: unknown) {
  if (error instanceof APIError && error.message === 'session_busy') return 'This chat is busy. Try again when it finishes.'
  if (error instanceof APIError && error.message === 'session_has_schedule') return 'This chat still has scheduled reminders. Remove them before deleting it.'
  return 'The chat could not be deleted. Try again.'
}

export function App() {
  const [userId, setUserID] = useState<string>()
  return <ExtensionDialogs>{userId ? <AccountChat userId={userId} onExit={() => setUserID(undefined)} /> : <AccountEntry onOpen={(id, created) => {
    if (created) window.history.pushState({ returnTo: '/' }, '', '/settings/models')
    setUserID(id)
  }} />}</ExtensionDialogs>
}

function AccountChat({ userId, onExit }: { userId: string; onExit: () => void }) {
  const { t, language } = useLocale()
  const [transcriptCache] = useState(() => new TranscriptCache())
  const [, updateSummaries] = useState(0)
  const [settingsPage, setSettingsPage] = useState(currentSettings)
  const [selectedPlugin, setSelectedPlugin] = useState<string>()
  const settingsOpen = settingsPage !== undefined
  const [settingsVersion, setSettingsVersion] = useState(0)
  const [hostReady, setHostReady] = useState(false)
  const [stoppingHost, setStoppingHost] = useState(false)
  const [stopHostError, setStopHostError] = useState('')
  const [hostAttempt, setHostAttempt] = useState(0)
  const [needsModel, setNeedsModel] = useState(false)
  const [rows, setRows] = useState<SessionRow[]>([])
  const [selected, setSelected] = useState(currentSession)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [filesOpen, setFilesOpen] = useState(false)
  const [compact, setCompact] = useState(() => window.matchMedia('(max-width: 1059px)').matches)
  const filesOverlay = filesOpen && compact
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [loading, setLoading] = useState(true)
  const [cursor, setCursor] = useState('')
  const loadedMore = useRef(false)
  const listedOnce = useRef(false)
  const createInFlight = useRef(false)
  const listRequest = useRef<AbortController | null>(null)
  const removed = useRef(new Set<string>())
  const settingsNav = useRef<HTMLElement>(null)

  useEffect(() => {
    const nav = settingsNav.current
    if (!nav) return
    const revealCurrent = () => {
      const current = nav.querySelector<HTMLElement>('[aria-current="page"]')
      if (!current) return
      const row = nav.getBoundingClientRect(), tab = current.getBoundingClientRect()
      const left = tab.left < row.left ? tab.left - row.left : tab.right > row.right ? tab.right - row.right : 0
      if (left) nav.scrollBy({ left, behavior: 'auto' })
    }
    revealCurrent()
    const resize = new ResizeObserver(revealCurrent)
    resize.observe(nav)
    return () => resize.disconnect()
  }, [settingsPage, language])

  async function exitHost() {
    setStoppingHost(true)
    setStopHostError('')
    try {
      await stopHost(userId)
      onExit()
    } catch (error) {
      setStoppingHost(false)
      setStopHostError(error instanceof APIError && error.status === 504
        ? 'Could not confirm the Host stopped. Try again to check its status.'
        : 'Could not stop your Host. Try again.')
    }
  }

  const refresh = useCallback(async (after = '') => {
    listRequest.current?.abort()
    const request = new AbortController()
    listRequest.current = request
    if (!listedOnce.current || after) setLoading(true)
    try {
      const page = await listSessions(userId, after, request.signal)
      if (request.signal.aborted) return
      if (!loadedMore.current || after) setCursor(page.nextCursor ?? '')
      if (after) loadedMore.current = true
      setRows(previous => {
        const merged = new Map(previous.filter(row => !transcriptCache.isUnavailable(row.sessionId)).map(row => [row.sessionId, row]))
        for (const row of page.sessions) if (!removed.current.has(row.sessionId)) merged.set(row.sessionId, row)
        return [...merged.values()].sort((a, b) => b.createdAt.localeCompare(a.createdAt) || a.sessionId.localeCompare(b.sessionId))
      })
    } catch {
      if (!request.signal.aborted) setError('Could not load chats.')
    } finally {
      if (!request.signal.aborted) {
        listedOnce.current = true
        setLoading(false)
      }
    }
  }, [userId, transcriptCache])

  useEffect(() => {
    if (!hostReady) return
    const connection = new HostConnection(transcriptCache, (cursors, signal, opened, receive) =>
      readHostStream(userId, cursors, signal, opened, receive))
    let refreshTimer: ReturnType<typeof setTimeout> | undefined
    const unsubscribe = transcriptCache.subscribe('@sessions', () => {
      clearTimeout(refreshTimer)
      refreshTimer = setTimeout(() => void refresh(), 150)
    })
    const unsubscribeSummaries = transcriptCache.subscribe('@summaries', () => updateSummaries(version => version + 1))
    connection.start()
    return () => { connection.stop(); unsubscribe(); unsubscribeSummaries(); clearTimeout(refreshTimer) }
  }, [hostReady, userId, transcriptCache, refresh])

  useEffect(() => {
    if (hostReady) return
    let active = true
    void openAccount(userId).then(() => {
      if (active) {
        setHostReady(true)
        setError('')
      }
    }, () => {
      if (active) {
        setLoading(false)
        setError('Could not start your Host. Try again.')
      }
    })
    return () => { active = false }
  }, [hostReady, hostAttempt, userId])

  useEffect(() => {
    if (hostReady) void refresh()
    function pop() {
      const settings = currentSettings()
      setSettingsPage(settings)
      if (settings) return
      setSettingsVersion(value => value + 1)
      setSelected(currentSession())
    }
    window.addEventListener('popstate', pop)
    return () => {
      listRequest.current?.abort()
      window.removeEventListener('popstate', pop)
    }
  }, [refresh, hostReady])

  useEffect(() => {
    const desktop = window.matchMedia('(min-width: 681px)')
    function closeAtDesktop() {
      if (desktop.matches) setDrawerOpen(false)
    }
    desktop.addEventListener('change', closeAtDesktop)
    return () => desktop.removeEventListener('change', closeAtDesktop)
  }, [])

  useEffect(() => {
    const layout = window.matchMedia('(max-width: 1059px)')
    function resize() { setCompact(layout.matches) }
    layout.addEventListener('change', resize)
    return () => layout.removeEventListener('change', resize)
  }, [])

  function closeDrawer() {
    setDrawerOpen(false)
    requestAnimationFrame(() => document.querySelector<HTMLElement>('.drawer-toggle')?.focus())
  }

  function closeFiles() {
    setFilesOpen(false)
    const selector = window.matchMedia('(max-width: 680px)').matches ? '.drawer-toggle' : '#account-menu-toggle'
    requestAnimationFrame(() => document.querySelector<HTMLElement>(selector)?.focus())
  }

  function toggleFiles() {
    if (filesOpen) {
      closeFiles()
      return
    }
    setDrawerOpen(false)
    setFilesOpen(true)
  }

  function choose(id?: string) {
    const complete = () => {
      const path = id ? '/chat/' + id : '/'
      if (window.location.pathname !== path) window.history.pushState(null, '', path)
      setSelected(id)
      setDrawerOpen(false)
      if (settingsOpen) {
        setSettingsPage(undefined)
        setSettingsVersion(value => value + 1)
      }
    }
    if (!settingsOpen || settingsNavigationAllowed(complete)) complete()
  }

  function unavailable(id: string) {
    if (selected !== id) return
    const path = '/chat/' + id
    if (window.location.pathname === path) window.history.replaceState(null, '', '/')
    else if (window.history.state?.returnTo === path) {
      window.history.replaceState({ returnTo: '/' }, '', window.location.href)
    }
    setRows(previous => previous.filter(row => row.sessionId !== id))
    setSelected(undefined)
    setError('This chat is no longer available.')
  }

  function showSettings() {
    if (settingsPage === 'plugins') { selectSettings('models'); setDrawerOpen(false); return }
    if (settingsOpen) {
      setDrawerOpen(false)
      return
    }
    window.history.pushState({ returnTo: window.location.pathname }, '', '/settings/models')
    setDrawerOpen(false)
    setSettingsPage('models')
  }

  function showPlugins() {
    const complete = () => {
      const returnTo = settingsOpen ? window.history.state?.returnTo || '/' : window.location.pathname
      window.history.pushState({ returnTo }, '', '/plugins')
      setSettingsPage('plugins')
      setDrawerOpen(false)
    }
    if (settingsPage === 'plugins') { setSelectedPlugin(undefined); setDrawerOpen(false); return }
    if (settingsNavigationAllowed(complete)) complete()
  }

  function selectSettings(page: SettingsPage) {
    if (page === settingsPage) return
    const complete = () => {
      window.history.replaceState(window.history.state, '', page === 'plugins' ? '/plugins' : '/settings/' + page)
      setSettingsPage(page)
    }
    if (settingsNavigationAllowed(complete)) complete()
  }

  function closeSettings() {
    if (settingsPage === 'plugins' && selectedPlugin) { setSelectedPlugin(undefined); return }
    const complete = () => {
      const target = typeof window.history.state?.returnTo === 'string' ? window.history.state.returnTo : '/'
      window.history.replaceState(null, '', target)
      setNeedsModel(false)
      setSettingsPage(undefined)
      setSettingsVersion(value => value + 1)
      requestAnimationFrame(() => document.getElementById('account-menu-toggle')?.focus())
    }
    if (settingsNavigationAllowed(complete)) complete()
  }

  function markTitle(id: string, text: string) {
    const clean = text.trim().replace(/\s+/g, ' ')
    const runes = Array.from(clean)
    const title = runes.length > 56 ? runes.slice(0, 55).join('') + '…' : clean
    setRows(items => items.map(row => row.sessionId === id && !row.title ? { ...row, title } : row))
  }

  async function start() {
    if (createInFlight.current || !hostReady) throw new Error('Host unavailable')
    createInFlight.current = true
    setCreating(true)
    setError('')
    setNeedsModel(false)
    try {
      const { sessionId } = await createSession(userId)
      setRows(previous => previous.some(row => row.sessionId === sessionId) ? previous
        : [{ sessionId, createdAt: new Date().toISOString() }, ...previous])
      choose(sessionId)
      void refresh()
    } catch (error) {
      if (error instanceof APIError && error.message === 'model_default_required') {
        setError('Set up an endpoint and a default model before creating a chat.')
        setNeedsModel(true)
      } else {
        setError('Could not confirm a new chat. Check the list before trying again.')
        void refresh()
      }
      throw error
    } finally {
      createInFlight.current = false
      setCreating(false)
    }
  }

  async function remove(row: SessionRow) {
    setError('')
    try {
      await archiveSession(userId, row.sessionId)
    } catch (error) {
      if (error instanceof APIError && ['session_has_schedule', 'session_busy'].includes(error.message)) {
        if (!window.confirm(t('Stop this chat and delete its active schedules? The chat history and files will be kept.'))) return false
        try {
          await archiveSession(userId, row.sessionId, true)
        } catch (stopError) {
          setError(errorDeleting(stopError))
          return false
        }
      } else if (!(error instanceof APIError && error.status === 404)) {
        setError(errorDeleting(error))
        return false
      }
    }
    removed.current.add(row.sessionId)
    transcriptCache.delete(row.sessionId)
    setRows(previous => previous.filter(item => item.sessionId !== row.sessionId))
    if (selected === row.sessionId) {
      if (settingsOpen) {
        if (window.history.state?.returnTo === '/chat/' + row.sessionId) {
          window.history.replaceState({ returnTo: '/' }, '', window.location.href)
        }
        setSelected(undefined)
      } else choose()
    }
    return true
  }

  return (
    <div className={filesOpen ? 'app app-files-open' : 'app'}>
      <Sidebar userId={userId} cache={transcriptCache} rows={rows} selected={selected} hasMore={Boolean(cursor)} isLoading={loading} creating={creating || !hostReady}
        filesOpen={filesOpen} filesAvailable={hostReady} filesOverlay={filesOverlay} onFiles={toggleFiles}
        hostReady={hostReady} stoppingHost={stoppingHost} stopHostError={stopHostError} onStopHost={exitHost}
        error={error} needsModel={needsModel} drawerOpen={drawerOpen} onSettings={showSettings} onPlugins={showPlugins} pluginsOpen={settingsPage === 'plugins'} onCreate={() => { void start().catch(() => {}) }} onChoose={choose}
        onDelete={remove} onMore={() => { void refresh(cursor) }} onClose={closeDrawer}
        onRetry={() => { setError(''); if (hostReady) void refresh(); else setHostAttempt(attempt => attempt + 1) }} />
      {settingsPage && <main className="model-settings" inert={drawerOpen || filesOverlay}>
        <div className="settings-topbar">
          <button type="button" className="icon-button drawer-toggle" onClick={() => setDrawerOpen(true)}
            aria-expanded={drawerOpen} aria-controls="chat-sidebar" aria-label={t('Open chats')}><Menu size={22} /></button>
        </div>
        <div className={settingsPage === 'plugins' ? 'model-settings-panel plugin-settings-panel' : 'model-settings-panel'}>
          <header className="settings-header">
            <div className="settings-header-actions">
              <button type="button" className="settings-back" onClick={closeSettings}><ArrowLeft size={17} /> {t(settingsPage === 'plugins' && selectedPlugin ? 'Plugin market' : 'Back to chats')}</button>
              <LanguagePicker />
            </div>
            {!(settingsPage === 'plugins' && selectedPlugin) && <h1>{t(settingsPage === 'plugins' ? 'Plugin market' : 'Settings')}</h1>}
          </header>
          {settingsPage !== 'plugins' && <nav ref={settingsNav} className="settings-nav" aria-label={t('Settings')}>
            <button type="button" aria-current={settingsPage === 'models' ? 'page' : undefined}
              onClick={() => selectSettings('models')}><SlidersHorizontal size={16} /> {t('Models')}</button>
            <button type="button" aria-current={settingsPage === 'web-search' ? 'page' : undefined}
              onClick={() => selectSettings('web-search')}><Globe size={16} /> {t('Web search')}</button>
            {imageGenerationSettingsEnabled && <button type="button" aria-current={settingsPage === 'image-generation' ? 'page' : undefined}
              onClick={() => selectSettings('image-generation')}><Image size={16} /> {t('Image generation')}</button>}
          </nav>}
          <div className="settings-content">
            {settingsPage === 'models' ? <Models userId={userId} /> : settingsPage === 'plugins' ? <Plugins userId={userId} cache={transcriptCache} selected={selectedPlugin} onSelect={setSelectedPlugin} />
              : <ToolProviders key={settingsPage} userId={userId} page={settingsPage} />}
          </div>
        </div>
      </main>}
      {hostReady && <Chat hidden={settingsOpen} userId={userId} cache={transcriptCache} settingsVersion={settingsVersion} onSettings={showSettings} drawerOpen={drawerOpen} filesOverlay={filesOverlay} key={selected ?? 'welcome'} id={selected}
        onOpenMenu={() => setDrawerOpen(true)} onAccepted={markTitle} onUnavailable={unavailable} onTitleRefresh={() => { void refresh() }} />}
      {hostReady && <HostFiles userId={userId} open={filesOpen} overlay={filesOverlay} onClose={closeFiles} />}
    </div>
  )
}
