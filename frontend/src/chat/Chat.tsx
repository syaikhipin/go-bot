import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { ArrowUp, FileText, LoaderCircle, Menu, Plus, Square, X } from 'lucide-react'
import { HarnessAvatar } from '../HarnessAvatar'
import { APIError, readTranscript, readStream, sendMessage, stopTurn, updateQueue } from '../api/controller'
import { MessageBubble } from './ChatContent'
import { ApprovalPanel, PermissionPicker } from './PermissionControls'
import { QuestionPanel } from './QuestionPanel'
import { addMessageFiles, isMessageImage } from './attachments'
import { ModelPicker } from './ModelPicker'
import { attemptMessage, emptyTranscript, groupAssistantReplies, messageText, textMessage, visibleQueue, type Message } from './transcript'
import { cacheTranscript, prependTranscriptPage, updateCachedTranscript, type TranscriptCache } from './transcriptCache'

function greeting() {
  const hour = new Date().getHours()
  if (hour < 12) return 'Good morning'
  if (hour < 18) return 'Good afternoon'
  return 'Good evening'
}

function sendError(error: unknown) {
  if (error instanceof APIError && error.message === 'model_default_required') return 'Set up an endpoint and a default model before creating a chat.'
  if (error instanceof APIError && (error.message === 'model_unconfigured' || error.message === 'model_unavailable')) return 'Choose an available model for this chat.'
  if (error instanceof APIError && error.message === 'host_model_settings_unavailable') return 'The Host could not apply the current model settings. Reopen the account or save the model settings again.'
  if (error instanceof APIError && error.status === 413) return 'The message or its attachments exceed the upload limit.'
  if (error instanceof APIError && error.message === 'invalid_image') return 'An attached image could not be read. Remove it and try again.'
  if (error instanceof APIError && error.status === 404) return 'This chat is no longer available.'
  if (error instanceof APIError && error.status < 500) return 'The message was not accepted. Check it and try again.'
  return 'Delivery could not be confirmed. Check the conversation before sending again.'
}

type ChatProps = {
  hidden?: boolean
  userId: string
  cache: TranscriptCache
  id?: string
  drawerOpen: boolean
  filesOverlay: boolean
  settingsVersion: number
  onSettings: () => void
  onOpenMenu: () => void
  onAccepted: (id: string, text: string) => void
  onUnavailable: (id: string) => void
  onTitleRefresh: () => void
}

export function Chat({ hidden, userId, cache, id, drawerOpen, filesOverlay, settingsVersion, onSettings, onOpenMenu, onAccepted, onUnavailable, onTitleRefresh }: ChatProps) {
  const cached = id ? cache.get(id) : undefined
  const [transcript, setTranscript] = useState(() => cached?.transcript ?? emptyTranscript())
  const [pending, setPending] = useState<Message[]>([])
  const [input, setInput] = useState('')
  const [files, setFiles] = useState<File[]>([])
  const [fileError, setFileError] = useState('')
  const filePicker = useRef<HTMLInputElement>(null)
  const [failure, setFailure] = useState('')
  const [historyError, setHistoryError] = useState('')
  const [connecting, setConnecting] = useState(false)
  const [loading, setLoading] = useState(Boolean(id && !cached))
  const [modelSaving, setModelSaving] = useState(false)
  const [permissionSaving, setPermissionSaving] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [awaitingMessage, setAwaitingMessage] = useState('')
  const [stopping, setStopping] = useState(false)
  const [before, setBefore] = useState<number | null>(() => cached?.before ?? null)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [olderError, setOlderError] = useState('')
  const [actionId, setActionId] = useState('')
  const [atBottom, setAtBottom] = useState(true)
  const scrollAnchor = useRef<{ height: number; top: number } | null>(null)
  const [loadAttempt, setLoadAttempt] = useState(0)
  const kick = useRef<() => void>(() => {})
  const historySignal = useRef<AbortSignal>(undefined)
  const composer = useRef<HTMLTextAreaElement>(null)
  const scroll = useRef<HTMLDivElement>(null)
  const following = useRef(true)
  const mounted = useRef(false)
  const submittingNow = useRef(false)
  const refresh = useRef(onTitleRefresh)
  refresh.current = onTitleRefresh
  const missing = useRef(onUnavailable)
  missing.current = onUnavailable

  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])

  useEffect(() => {
    if (!id) return
    const sessionId = id
    const abort = new AbortController()
    const signal = abort.signal
    historySignal.current = signal
    setLoadingOlder(false)
    let entry = cache.get(sessionId)
    let running = false
    let retryTimer: number | undefined
    let wait = 500

    function scheduleReconnect() {
      retryTimer = window.setTimeout(() => { void follow() }, wait)
      wait = Math.min(wait * 2, 4000)
    }

    async function follow() {
      if (running || !entry || signal.aborted) return
      const current = entry
      window.clearTimeout(retryTimer)
      running = true
      try {
        await readStream(userId, sessionId, current.cursor, signal, () => {
          if (signal.aborted) return
          wait = 500
          setConnecting(false)
        }, (kind, data) => {
          if (signal.aborted || cache.get(sessionId) !== current) return
          setTranscript(updateCachedTranscript(current, kind, data))
        })
        if (!signal.aborted) {
          setConnecting(true)
          scheduleReconnect()
        }
      } catch (error) {
        if (signal.aborted) return
        if (error instanceof APIError && error.status === 404) {
          cache.delete(sessionId)
          entry = undefined
          missing.current(sessionId)
        } else {
          setConnecting(true)
          scheduleReconnect()
        }
      } finally {
        running = false
      }
    }

    kick.current = () => { void follow() }

    async function loadHistory() {
      setLoading(true)
      setHistoryError('')
      try {
        const page = await readTranscript(userId, sessionId, null, signal)
        if (signal.aborted) return
        entry = cacheTranscript(page)
        cache.keep(sessionId, entry)
        setBefore(entry.before)
        setTranscript(entry.transcript)
        if (entry.cursor !== null) refresh.current()
        void follow()
      } catch (error) {
        if (signal.aborted) return
        if (error instanceof APIError && error.status === 404) missing.current(sessionId)
        else setHistoryError('Chat history could not be loaded.')
      } finally {
        if (!signal.aborted) setLoading(false)
      }
    }

    if (entry) {
      cache.keep(sessionId, entry)
      setHistoryError('')
      void follow()
    } else void loadHistory()
    return () => {
      abort.abort()
      window.clearTimeout(retryTimer)
      kick.current = () => {}
    }
  }, [cache, id, loadAttempt, userId])

  const pendingMessages = pending.filter(item => !transcript.messages.some(message => message.id === item.id))
  const displayedMessages = [...transcript.messages, ...pendingMessages]
  const queue = visibleQueue(transcript, displayedMessages)
  const draft = transcript.draft ? attemptMessage(transcript.draft) : null
  if (draft?.blocks.length) displayedMessages.push(draft)
  const replies = groupAssistantReplies(displayedMessages)
  const activeTurn = transcript.activeTurn
  const hasReply = activeTurn !== null && replies.some(message => message.role === 'assistant' && message.turn === activeTurn)
  const messageObserved = transcript.messages.some(message => message.id === awaitingMessage) ||
    transcript.queue.some(message => message.id === awaitingMessage)
  const waiting = activeTurn !== null ? !hasReply && transcript.questions.length === 0
    : Boolean(awaitingMessage && !messageObserved)
  const generating = activeTurn !== null && transcript.questions.length === 0
  const showWelcome = !loading && !historyError && transcript.messages.length === 0 && pendingMessages.length === 0 &&
    !draft && !waiting && activeTurn === null && transcript.questions.length === 0
  useEffect(() => {
    if (awaitingMessage && (activeTurn !== null || messageObserved)) {
      setAwaitingMessage('')
    }
  }, [awaitingMessage, activeTurn, messageObserved])
  useEffect(() => {
    setPending(items => {
      const remaining = items.filter(item => !transcript.messages.some(message => message.id === item.id))
      return remaining.length === items.length ? items : remaining
    })
  }, [transcript.messages])
  useEffect(() => {
    if (!hidden && !drawerOpen && !filesOverlay && window.matchMedia('(min-width: 681px)').matches &&
        (document.activeElement === document.body || document.activeElement === composer.current)) {
      composer.current?.focus({ preventScroll: true })
    }
  }, [showWelcome, hidden, drawerOpen, filesOverlay])
  useLayoutEffect(() => {
    const panel = scroll.current
    if (!panel) return
    if (scrollAnchor.current) {
      panel.scrollTop = scrollAnchor.current.top + panel.scrollHeight - scrollAnchor.current.height
      scrollAnchor.current = null
    } else if (following.current) {
      panel.scrollTop = panel.scrollHeight
    }
  }, [transcript, pending.length, waiting, files.length])

  async function loadOlder() {
    const entry = id ? cache.get(id) : undefined
    const signal = historySignal.current
    if (!id || !entry || !signal || entry.before === null || loadingOlder) return
    setLoadingOlder(true)
    setOlderError('')
    try {
      const page = await readTranscript(userId, id, entry.before, signal)
      if (signal.aborted || cache.get(id) !== entry) return
      const panel = scroll.current
      if (panel) scrollAnchor.current = { height: panel.scrollHeight, top: panel.scrollTop }
      prependTranscriptPage(entry, page)
      setBefore(entry.before)
      setTranscript(entry.transcript)
    } catch {
      if (!signal.aborted) setOlderError('Older messages could not be loaded.')
    } finally {
      if (!signal.aborted) setLoadingOlder(false)
    }
  }

  async function queueAction(messageId: string, action: 'steer' | 'delete') {
    if (!id || actionId) return
    setActionId(messageId)
    setFailure('')
    try {
      await updateQueue(userId, id, messageId, action)
    } catch {
      if (mounted.current) setFailure('The queued message could not be updated.')
    } finally {
      if (mounted.current) setActionId('')
    }
  }

  async function stopGenerating() {
    if (!id || activeTurn === null || stopping) return
    setStopping(true)
    try {
      await stopTurn(userId, id, activeTurn)
    } catch {
      if (mounted.current) setFailure('Generation could not be stopped.')
    } finally {
      if (mounted.current) setStopping(false)
    }
  }

  function addFiles(incoming: File[]) {
    if (!incoming.length || submittingNow.current) return
    const next = addMessageFiles(files, incoming)
    setFiles(next.files)
    setFileError(next.error)
  }

  async function submit() {
    const text = input.trim()
    if (!id || (!text && !files.length) || submittingNow.current || modelSaving || permissionSaving || loading || historyError) return
    submittingNow.current = true
    setSubmitting(true)
    setFailure('')
    try {
      const showImmediately = transcript.activeTurn === null && transcript.queue.length === 0 && pendingMessages.length === 0
      const result = await sendMessage(userId, id, text, files)
      onAccepted(id, text)
      if (!mounted.current) return
      if (showImmediately) {
        setAwaitingMessage(result.messageId)
        if (!files.length) setPending(items => [...items, textMessage(result.messageId, text)])
      }
      kick.current()
      setInput(value => value.trim() === text ? '' : value)
      setFiles([])
      setFileError('')
    } catch (error) {
      if (mounted.current) setFailure(sendError(error))
    } finally {
      submittingNow.current = false
      if (mounted.current) {
        setSubmitting(false)
        composer.current?.focus({ preventScroll: true })
      }
    }
  }

  const form = (
    <form className="composer" onSubmit={event => { event.preventDefault(); void submit() }}
      onPaste={event => {
        const listed = Array.from(event.clipboardData.files)
        const incoming = listed.length ? listed : Array.from(event.clipboardData.items).flatMap(item => {
          const file = item.kind === 'file' ? item.getAsFile() : null
          return file ? [file] : []
        })
        if (!incoming.length) return
        event.preventDefault()
        addFiles(incoming)
      }}>
      <input type="file" multiple hidden ref={filePicker} onChange={event => {
        addFiles(Array.from(event.currentTarget.files ?? []))
        event.currentTarget.value = ''
        composer.current?.focus()
      }} />
      {files.length > 0 && <ul className="draft-files" aria-label="Attachments to send">
        {files.map((file, index) => <li key={file.name + ':' + file.lastModified + ':' + index}>
          <DraftFilePreview file={file} />
          <span className="draft-file-label"><strong title={file.name}>{file.name}</strong>
            <small>{file.size >= 1048576 ? (file.size / 1048576).toFixed(1) + ' MB' : Math.max(1, Math.round(file.size / 1024)) + ' KB'}</small>
          </span>
          <button type="button" disabled={submitting} aria-label={'Remove ' + file.name} onClick={() => {
            setFiles(current => current.filter((_, i) => i !== index))
            setFileError('')
          }}><X size={15} /></button>
        </li>)}
      </ul>}
      {fileError && <p role="alert" className="feedback attachment-error">{fileError}</p>}
      <label className="sr-only" htmlFor="message">Message</label>
      <textarea id="message" ref={composer} value={input} rows={2} placeholder="Ask anything…"
        onChange={event => setInput(event.target.value)}
        onKeyDown={event => {
          if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && event.keyCode !== 229) {
            event.preventDefault()
            void submit()
          }
        }}
      />
      <div className="composer-actions">
        <div className="composer-tools">
          <button type="button" className="attachment-button" aria-label="Add attachments" title="Add attachments"
            disabled={submitting} onClick={() => filePicker.current?.click()}><Plus size={21} /></button>
          <ModelPicker userId={userId} id={id} settingsVersion={settingsVersion} onSettings={onSettings} onSaving={setModelSaving} />
          {id && <PermissionPicker key={id} userId={userId} sessionId={id} streamState={transcript.permission}
            disabled={activeTurn !== null || submitting} onSaving={setPermissionSaving} />}
        </div>
        <div className="composer-submit">
          {activeTurn !== null && <button className="stop-generation" type="button" title="Stop generating"
            aria-label="Stop generating" disabled={stopping} onClick={() => { void stopGenerating() }}>
            {stopping ? <LoaderCircle className="spinner" size={16} /> : <Square size={15} />}
          </button>}
          <button className="send-button" type="submit" aria-label="Send message"
            disabled={(!input.trim() && !files.length) || submitting || modelSaving || permissionSaving || loading || Boolean(historyError)}>
            {submitting ? <LoaderCircle className="spinner" size={20} /> : <ArrowUp size={21} />}
          </button>
        </div>
      </div>
    </form>
  )

  return (
    <main className="workspace" hidden={hidden} inert={drawerOpen || filesOverlay}>
      <header className="topbar">
        <button className="icon-button drawer-toggle" type="button" onClick={onOpenMenu} aria-expanded={drawerOpen} aria-controls="chat-sidebar" aria-label="Open chats"><Menu size={22} /></button>
        <span>Go Harness Chat</span>
      </header>
      {!id ? (
        <div className="chat-empty">
          <HarnessAvatar className="welcome-avatar" />
        </div>
      ) : showWelcome ? (
        <div className="welcome">
          <div className="welcome-copy">
            <HarnessAvatar className="welcome-avatar" />
            <h1>{greeting()}</h1>
            <p>How can I help you today?</p>
          </div>
          <div className="welcome-input">{form}{failure && <p className="feedback" role="alert">{failure}</p>}</div>
        </div>
      ) : (
        <div className="conversation">
          <div className="transcript-scroll" ref={scroll} onScroll={event => {
            const panel = event.currentTarget
            following.current = panel.scrollHeight - panel.scrollTop - panel.clientHeight < 88
            setAtBottom(following.current)
          }}>
            <div className="transcript" aria-label="Conversation">
              {loading && <p className="chat-status" role="status">Loading chat…</p>}
              {historyError && <div className="chat-status" role="alert">{historyError} <button type="button" className="text-button" onClick={() => setLoadAttempt(attempt => attempt + 1)}>Retry</button></div>}
              {before !== null && <div className="history-actions">
                <button type="button" className="text-button" onClick={() => { void loadOlder() }} disabled={loadingOlder}>
                  {loadingOlder ? 'Loading…' : 'Load earlier messages'}
                </button>
                {olderError && <p className="feedback" role="alert">{olderError}</p>}
              </div>}
              {replies.map(message => <MessageBubble key={message.id} message={message} tools={transcript.toolResults} userId={userId} sessionId={id} />)}
              {!loading && !historyError && (waiting || generating) && <div className={waiting ? 'generation-row' : 'generation-row generation-follow'} role="status">
                {waiting && <HarnessAvatar className="reply-mark" />}
                <span className="generation-copy"><span className="generation-dots" aria-hidden="true"><i /><i /><i /></span>{waiting ? 'Preparing a reply…' : 'Generating…'}</span>
              </div>}
            </div>
          </div>
          <div className="chat-input">
            {connecting && <p className="chat-status reconnecting" role="status">Reconnecting…</p>}
            {failure && <p className="feedback" role="alert">{failure}</p>}
            {!atBottom && <button type="button" className="text-button" onClick={() => {
              const panel = scroll.current
              if (panel) panel.scrollTop = panel.scrollHeight
            }}>Back to latest</button>}
            {queue.length > 0 && <ul className="message-queue" aria-label="Queued messages">
              {queue.map(item => <li key={item.id}>
                <span className="queue-summary">{messageText(item) || 'Attachment'}</span>
                {item.placement === 'steering' ? <small>Waiting to steer</small> : <>
                  <button type="button" className="text-button" disabled={actionId === item.id}
                    onClick={() => { void queueAction(item.id, 'steer') }}>Steer</button>
                  <button type="button" className="text-button" disabled={actionId === item.id}
                    onClick={() => { void queueAction(item.id, 'delete') }}>Delete</button>
                </>}
              </li>)}
            </ul>}
            {id && transcript.questions.length > 0 && <div className="question-dock">
              {transcript.questions.map(item => item.approval
                ? <ApprovalPanel key={item.callId} item={item} userId={userId} sessionId={id} />
                : <QuestionPanel key={item.callId} item={item} userId={userId} sessionId={id} />)}
            </div>}
            {form}
          </div>
        </div>
      )}
    </main>
  )
}



function DraftFilePreview({ file }: { file: File }) {
  const [src, setSrc] = useState('')
  useEffect(() => {
    if (!isMessageImage(file)) return
    const url = URL.createObjectURL(file)
    setSrc(url)
    return () => URL.revokeObjectURL(url)
  }, [file])
  return <span className="draft-file-preview">{isMessageImage(file) && src
    ? <img src={src} alt="" onError={() => setSrc('')} />
    : <FileText size={17} aria-hidden="true" />}</span>
}
