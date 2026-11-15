import { useEffect, useState } from 'react'
import { Check, ChevronRight, CircleAlert, FileDown, LoaderCircle, Terminal, Wrench } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { fetchChatFile } from '../api/controller'
import { HarnessAvatar } from '../HarnessAvatar'
import { groupDisplayBlocks, messageText, type AttachmentRef, type FileRef, type Message, type ToolBlock, type Transcript } from './transcript'

type SessionAddress = { userId: string; sessionId?: string }

export function MessageBubble({ message, tools, ...address }: SessionAddress & { message: Message; tools: Transcript['toolResults'] }) {
  if (message.role === 'notice') return <p className="chat-status">{messageText(message)}</p>
  const isAssistant = message.role === 'assistant'
  return (
    <article className={'message ' + (isAssistant ? 'assistant-message' : 'user-message')}>
      {isAssistant && <HarnessAvatar className="reply-mark" />}
      <div className={isAssistant ? 'message-blocks markdown' : 'message-blocks'}>
        {groupDisplayBlocks(message.blocks, tools).map(block => {
          switch (block.type) {
            case 'text':
              return isAssistant ? <MarkdownText key={block.id} text={block.text} /> : <div key={block.id}>{block.text}</div>
            case 'tool':
              return <ToolCard key={block.id} block={block} progress={tools[block.callId]} />
            case 'tool-group':
              return <ToolGroup key={block.id} blocks={block.tools} tools={tools} />
            case 'file':
              return <FileBlock key={block.id} name={block.name} description={block.description} file={block.file} {...address} />
            case 'image-row':
              return <div key={block.id} className="chat-image-row">
                {block.images.map(image => <ImageBlock key={image.id} attachment={image.attachment} {...address} />)}
              </div>
          }
        })}
      </div>
    </article>
  )
}

function MarkdownText({ text }: { text: string }) {
  return <div>
    <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{
      a: ({ children, href }) => href && /^(https?:|mailto:|#)/i.test(href)
        ? <a href={href} target="_blank" rel="noreferrer">{children}</a>
        : <span>{children}</span>,
      img: ({ node, alt }) => {
        const start = node?.position?.start.offset
        const end = node?.position?.end.offset
        return start !== undefined && end !== undefined ? text.slice(start, end) : alt
      },
      table: ({ children }) => <div className="markdown-table-scroll" role="region" aria-label="Table" tabIndex={0}>
        <table>{children}</table>
      </div>,
    }}>{text}</ReactMarkdown>
  </div>
}

const toolLabels: Record<string, string> = {
  read_file: 'Read file', write_file: 'Write file', edit_file: 'Edit file',
  readimage: 'Read image', generateimage: 'Generate image',
  bash: 'Run command', shell: 'Run command', web_search: 'Search the web', web_fetch: 'Open web page',
}

function ToolGroup({ blocks, tools }: { blocks: ToolBlock[]; tools: Transcript['toolResults'] }) {
  const statuses = blocks.map(block => tools[block.callId]?.status)
  const remaining = statuses.filter(status => !status).length
  const issues = statuses.filter(status => status === 'failed' || status === 'cancelled').length
  return <details className="tool-card tool-group">
    <summary>
      <Wrench size={15} aria-hidden="true" />
      <span>{blocks.length} tool steps</span>
      {remaining > 0 ? <small>{remaining} running</small> : issues > 0 ? <small>{issues} interrupted</small> : <Check size={15} className="tool-state done" aria-label="Finished" />}
      <ChevronRight size={15} className="tool-chevron" aria-hidden="true" />
    </summary>
    <div className="tool-group-items">
      {blocks.map(block => <ToolCard key={block.id} block={block} progress={tools[block.callId]} />)}
    </div>
  </details>
}

function ToolCard({ block, progress }: { block: ToolBlock; progress?: Transcript['toolResults'][string] }) {
  const label = toolLabels[block.name] ?? block.name.replace(/[_-]+/g, ' ')
  const status = progress?.status
  const outcome = status === 'succeeded' ? 'Done' : status === 'failed' ? 'Failed' : status === 'cancelled' ? 'Stopped' : 'Running'
  const duration = progress?.startedAt !== undefined && progress.endedAt !== undefined
    ? Math.max(0, progress.endedAt - progress.startedAt) : undefined
  const timing = duration === undefined ? '' : duration < 1000 ? duration + ' ms' : (duration / 1000).toFixed(1) + ' s'
  const row = <><Terminal size={14} aria-hidden="true" /><span>{label}</span>
    {status === 'succeeded' ? <Check size={14} className="tool-state done" aria-label={outcome} />
      : status ? <CircleAlert size={14} className="tool-state issue" aria-label={outcome} />
        : <LoaderCircle size={14} className="tool-state spinner" aria-label={outcome} />}</>
  if (!timing && !progress?.code) return <div className="tool-card"><div className="tool-summary">{row}</div></div>
  return <details className="tool-card">
    <summary>{row}<ChevronRight size={14} className="tool-chevron" aria-hidden="true" /></summary>
    <div className="tool-detail">{[timing && 'Duration ' + timing, progress?.code?.replaceAll('_', ' ').toLowerCase()].filter(Boolean).join(' · ')}</div>
  </details>
}

function FileBlock({ name, description, file, userId, sessionId }: SessionAddress & { name?: string; description?: string; file: FileRef }) {
  const filename = name || 'Attachment'
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function download() {
    if (!sessionId) return
    setBusy(true)
    setError('')
    try {
      const blob = await fetchChatFile(userId, sessionId, file, name)
      const href = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = href
      link.download = filename
      link.click()
      window.setTimeout(() => URL.revokeObjectURL(href), 1000)
    } catch {
      setError('File unavailable.')
    } finally {
      setBusy(false)
    }
  }
  return <div>
    <button type="button" className="chat-file" disabled={busy || !sessionId} onClick={() => { void download() }}>
      <FileDown size={16} aria-hidden="true" /> <span>{filename}</span>
    </button>
    {description && <div className="chat-file-description">{description}</div>}
    {error && <div className="feedback" role="status">{error}</div>}
  </div>
}

function ImageBlock({ attachment, userId, sessionId }: SessionAddress & { attachment: AttachmentRef }) {
  const { attachmentId, name, width, height } = attachment
  const [href, setHref] = useState('')
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    if (!sessionId) return
    const abort = new AbortController()
    let objectURL = ''
    void fetchChatFile(userId, sessionId, { source: 'attachment', attachmentId }, name, abort.signal).then(blob => {
      if (abort.signal.aborted) return
      objectURL = URL.createObjectURL(blob)
      setHref(objectURL)
    }).catch(() => {
      if (!abort.signal.aborted) setFailed(true)
    })
    return () => {
      abort.abort()
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
  }, [userId, sessionId, attachmentId, name])
  if (failed) return <span className="chat-status">Image unavailable.</span>
  if (!href) return <div className="chat-image-placeholder" aria-label="Loading image" />
  return <a href={href} target="_blank" rel="noreferrer" aria-label={name || 'Open image'}>
    <img className="chat-image" src={href} alt={name || 'Attachment'} width={width} height={height} onError={() => setFailed(true)} />
  </a>
}
