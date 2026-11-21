import { useEffect, useRef, useState } from 'react'
import { useLocale } from '../locale'
import { ChevronDown, Ellipsis, FolderTree, MessageSquare, MessageSquarePlus, Power, Puzzle, Settings, Trash2, X } from 'lucide-react'
import { HarnessAvatar } from '../HarnessAvatar'
import type { SessionRow } from '../api/controller'
import type { TranscriptCache } from '../chat/transcriptCache'

type Props = {
  cache: TranscriptCache
  userId: string
  rows: SessionRow[]
  selected?: string
  hasMore: boolean
  isLoading: boolean
  creating: boolean
  error: string
  needsModel: boolean
  onSettings: () => void
  onPlugins: () => void
  pluginsOpen: boolean
  filesOpen: boolean
  filesAvailable: boolean
  filesOverlay: boolean
  onFiles: () => void
  hostReady: boolean
  stoppingHost: boolean
  stopHostError: string
  onStopHost: () => Promise<void>
  drawerOpen: boolean
  onCreate: () => void
  onChoose: (id: string) => void
  onDelete: (session: SessionRow) => Promise<boolean>
  onMore: () => void
  onRetry: () => void
  onClose: () => void
}

type ContextMenu = { row: SessionRow; x: number; y: number; anchor: HTMLElement }

export function Sidebar(props: Props) {
  const { language, t } = useLocale()
  const rowTitle = (row: SessionRow) => row.isMain ? 'Go Bot' : row.title || t('New chat')
  const [menu, setMenu] = useState<ContextMenu | null>(null)
  const [accountOpen, setAccountOpen] = useState(false)
  const accountDock = useRef<HTMLDivElement>(null)
  const accountButton = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    if (!accountOpen) return
    function outside(event: PointerEvent) {
      if (!accountDock.current?.contains(event.target as Node)) setAccountOpen(false)
    }
    document.addEventListener('pointerdown', outside)
    return () => document.removeEventListener('pointerdown', outside)
  }, [accountOpen])

  const [confirm, setConfirm] = useState<ContextMenu | null>(null)
  const [deleting, setDeleting] = useState(false)
  const menuRef = useRef<HTMLButtonElement>(null)
  const dialog = useRef<HTMLDialogElement>(null)
  const drawerClose = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    if (props.drawerOpen) drawerClose.current?.focus()
    else setAccountOpen(false)
  }, [props.drawerOpen])

  useEffect(() => {
    if (!menu) return
    menuRef.current?.focus()
    function dismiss(event: PointerEvent) {
      if (!menuRef.current?.contains(event.target as Node)) setMenu(null)
    }
    function outsideViewport() { setMenu(null) }
    window.addEventListener('pointerdown', dismiss)
    window.addEventListener('resize', outsideViewport)
    return () => {
      window.removeEventListener('pointerdown', dismiss)
      window.removeEventListener('resize', outsideViewport)
    }
  }, [menu])

  useEffect(() => {
    if (confirm) dialog.current?.showModal()
    else dialog.current?.close()
  }, [confirm])

  function openMenu(row: SessionRow, anchor: HTMLElement, x: number, y: number) {
    if (row.isMain) return
    setMenu({ row, anchor, x: Math.max(8, Math.min(x, window.innerWidth - 188)), y: Math.max(8, Math.min(y, window.innerHeight - 60)) })
  }

  function closeConfirm(restoreFocus: boolean) {
    if (restoreFocus) {
      const anchor = confirm?.anchor
      requestAnimationFrame(() => anchor?.focus())
    }
    setConfirm(null)
  }

  return (
    <>
      {props.drawerOpen && <button type="button" className="scrim" onClick={props.onClose} aria-label={t('Close chats')} />}
      <aside id="chat-sidebar" className={props.drawerOpen ? 'sidebar drawer-open' : 'sidebar'} aria-label={t('Chats')}
        inert={props.filesOverlay}
        onKeyDown={event => { if (event.key === 'Escape' && !menu && !event.defaultPrevented) props.onClose() }}>
        <div className="brand">
          <HarnessAvatar className="brand-mark" />
          <span>Go Bot</span>
          <button ref={drawerClose} type="button" className="icon-button drawer-close" onClick={props.onClose} aria-label={t('Close chats')}><X size={21} /></button>
        </div>
        <button type="button" className="new-chat" onClick={props.onCreate} disabled={props.creating}>
          <MessageSquarePlus size={21} strokeWidth={1.75} /><span>{t('New chat')}</span>
        </button>
        <div className="section-heading">{t('Chats')}</div>
        <div className="session-list">
          {props.error && <p className="sidebar-feedback" role="alert">{t(props.error)} <button type="button" className="text-button" onClick={props.needsModel ? props.onSettings : props.onRetry}>{props.needsModel ? t('Open model settings') : t('Retry')}</button></p>}
          {!props.error && !props.isLoading && props.rows.length === 0 && <p className="sidebar-feedback">{t('No chats yet')}</p>}
          {props.rows.map(row => (
            <div className={row.sessionId === props.selected ? 'chat-row selected' : 'chat-row'} key={row.sessionId}
              onContextMenu={event => {
                event.preventDefault()
                const anchor = event.currentTarget.querySelector('button')!
                openMenu(row, anchor, event.clientX, event.clientY)
              }}>
              <button type="button" className="chat-link" aria-current={row.sessionId === props.selected ? 'page' : undefined}
                title={rowTitle(row)} onClick={() => props.onChoose(row.sessionId)}
                onKeyDown={event => {
                  if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
                    event.preventDefault()
                    const box = event.currentTarget.getBoundingClientRect()
                    openMenu(row, event.currentTarget, box.left + 12, box.bottom)
                  }
                }}>
                <MessageSquare size={16} strokeWidth={1.65} />
                <span className="chat-label">{rowTitle(row)}{row.isMain && <small className="main-chat-label">{t('Main chat')}</small>}
                  {props.cache.summary(row.sessionId)?.pending && <small className="session-attention"><span aria-hidden="true" />{t(props.cache.summary(row.sessionId)?.pending === 'approval' ? 'Approval needed' : 'Answer needed')}</small>}
                  {!row.title && <small>{new Date(row.createdAt).toLocaleString(language, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</small>}
                </span>
              </button>
              {!row.isMain && <button type="button" className="icon-button row-actions" aria-label={t('Actions for {name}', { name: rowTitle(row) })} aria-haspopup="menu"
                onClick={event => {
                  const box = event.currentTarget.getBoundingClientRect()
                  openMenu(row, event.currentTarget, box.right - 168, box.bottom)
                }}>
                <Ellipsis size={18} />
              </button>}
            </div>
          ))}
          {props.hasMore && <button type="button" className="text-button more" disabled={props.isLoading} onClick={props.onMore}>{t('Load more')}</button>}
        </div>
        <div className="sidebar-footer">
        <button id="plugin-market-link" type="button" className="sidebar-destination" aria-current={props.pluginsOpen ? 'page' : undefined} onClick={props.onPlugins}><Puzzle size={19} strokeWidth={1.7} /><span>{t('Plugin market')}</span></button>
        <div className="account-dock" ref={accountDock} onBlur={event => {
          if (!event.currentTarget.contains(event.relatedTarget)) setAccountOpen(false)
        }} onKeyDown={event => {
          if (event.key === 'Escape' && accountOpen) {
            event.preventDefault()
            setAccountOpen(false)
            accountButton.current?.focus()
          }
        }}>
          <button ref={accountButton} type="button" id="account-menu-toggle" className="account-trigger"
            aria-expanded={accountOpen} aria-controls="account-actions" onClick={() => setAccountOpen(value => !value)}>
            <span className="account-avatar" aria-hidden="true">{Array.from(props.userId)[0]?.toUpperCase()}</span>
            <span className="account-identity"><strong title={props.userId}>{props.userId}</strong>
              <small>{props.hostReady ? t('Personal workspace') : t('Host unavailable')}</small></span>
            <ChevronDown size={16} aria-hidden="true" className="account-chevron" />
          </button>
          {accountOpen && <div id="account-actions" className="account-popover" role="group" aria-label={t('Account actions')}>
            <button type="button" onClick={() => { setAccountOpen(false); props.onSettings() }}>
              <Settings size={18} aria-hidden="true" /> {t('Settings')}
            </button>
            <button type="button" onClick={() => { setAccountOpen(false); props.onFiles() }}
              disabled={!props.filesAvailable} aria-controls="host-files" aria-expanded={props.filesOpen}>
              <FolderTree size={18} aria-hidden="true" /> {t('File system')}
            </button>
            <div className="account-menu-divider" />
            <button type="button" className="stop-host" onClick={() => { void props.onStopHost() }}
              disabled={!props.hostReady || props.stoppingHost}>
              <Power size={18} aria-hidden="true" /> {props.stoppingHost ? t('Stopping Host…') : t('Stop Host')}
            </button>
          </div>}
          {props.stopHostError && <p className="sidebar-feedback" role="alert">{t(props.stopHostError)}</p>}
        </div>
        </div>
      </aside>
      {menu && (
        <div className="context-menu" role="menu" aria-label={t('Chat actions')}
          ref={node => { if (node) { node.style.left = menu.x + 'px'; node.style.top = menu.y + 'px' } }}>
          <button ref={menuRef} type="button" role="menuitem"
            onKeyDown={event => {
              if (event.key === 'Escape' || event.key === 'Tab') {
                event.preventDefault()
                menu.anchor.focus()
                setMenu(null)
              }
            }}
            onClick={() => { setConfirm(menu); setMenu(null) }}>
            <Trash2 size={16} /> {t('Delete chat')}
          </button>
        </div>
      )}
      <dialog ref={dialog} className="archive-dialog" aria-labelledby="archive-title" aria-describedby="archive-copy"
        onCancel={event => { event.preventDefault(); if (!deleting) closeConfirm(true) }}>
        {confirm && (
          <>
            <h2 id="archive-title">{t('Delete chat?')}</h2>
            <p id="archive-copy"><strong>{rowTitle(confirm.row)}</strong> {t('will disappear from the chat list. Its data will stay on disk.')}</p>
            <div className="dialog-actions">
              <button type="button" autoFocus disabled={deleting} onClick={() => closeConfirm(true)}>{t('Cancel')}</button>
              <button type="button" className="danger" disabled={deleting} onClick={async () => {
                setDeleting(true)
                try {
                  const removed = await props.onDelete(confirm.row)
                  closeConfirm(!removed)
                  if (removed) requestAnimationFrame(() => document.querySelector<HTMLElement>('.new-chat')?.focus())
                } finally { setDeleting(false) }
              }}>{deleting ? t('Deleting…') : t('Delete chat')}</button>
            </div>
          </>
        )}
      </dialog>
    </>
  )
}
