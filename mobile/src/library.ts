import { Directory, File, Paths } from 'expo-file-system'
import * as Photos from 'expo-image-picker'
import * as Documents from 'expo-document-picker'
import * as Sharing from 'expo-sharing'
import { Platform } from 'react-native'
import Share from 'react-native-share'
import { Asset, requestPermissionsAsync } from 'expo-media-library'
import { sha256 } from 'js-sha256'
import { ImageManipulator, SaveFormat } from 'expo-image-manipulator'

export type Kind = 'image' | 'file'
export type LibraryEntry = { id: string; name: string; uri: string; kind: Kind; size: number; savedAt: number }
type Source = { uri: string; name: string; mimeType?: string }
export type RemoteFile = Source & { headers: Record<string, string> }
export type WorkspaceFileSource = { account: string; path: string }
const remoteSaves = new Map<string, Promise<LibraryEntry>>()
const subscribers = { image: new Set<() => void>(), file: new Set<() => void>() }
export function subscribeLibrary(kind: Kind, notify: () => void) {
  subscribers[kind].add(notify)
  return () => { subscribers[kind].delete(notify) }
}
const acceptedPhoto = /\.(gif|jpe?g|png|webp)$/i
const photoExtensions = /\.(gif|jpe?g|png|webp|heic|heif|avif|bmp|tiff?)$/i
const root = (kind: Kind) => new Directory(Paths.document, 'library', kind)
const nonce = () => Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10)
const safeName = (name: string) => name.replace(/[\u0000-\u001f/\\]/g, '_').trim().slice(0, 180).replace(/^\.+$/, '') || 'attachment'

function firstFile(folder: Directory) {
  return folder.exists ? folder.list().find((item): item is File => item instanceof File) : undefined
}
function entry(file: File, id: string, kind: Kind): LibraryEntry {
  return { id, name: file.name, uri: file.uri, size: file.size, kind, savedAt: file.lastModified ?? 0 }
}
export function readLibrary(kind: Kind) {
  const directory = root(kind)
  if (!directory.exists) return []
  return directory.list().flatMap(folder => {
    if (!(folder instanceof Directory)) return []
    const file = firstFile(folder)
    return file ? [entry(file, folder.name, kind)] : []
  }).sort((a, b) => b.savedAt - a.savedAt || b.id.localeCompare(a.id))
}

async function save(source: Source, kind: Kind, id = nonce(), move = false): Promise<LibraryEntry> {
  const folder = new Directory(root(kind), id)
  let converted: File | undefined
  try {
    let input = new File(source.uri)
    let name = safeName(source.name)
    if (kind === 'image' && !acceptedPhoto.test(name)) {
      const extension = ({ 'image/png': 'png', 'image/jpeg': 'jpg', 'image/gif': 'gif', 'image/webp': 'webp' } as Record<string, string>)[source.mimeType ?? '']
      if (extension) {
        name = name.replace(/\.[^.]+$/, '') + '.' + extension
      } else {
        const rendered = await ImageManipulator.manipulate(source.uri).renderAsync()
        converted = new File((await rendered.saveAsync({ format: SaveFormat.JPEG, compress: 0.92 })).uri)
        input = new File(converted.uri)
        name = name.replace(/\.[^.]+$/, '') + '.jpg'
      }
    }
    folder.create({ intermediates: true, idempotent: true })
    const destination = new File(folder, name)
    await (move ? input.move(destination) : input.copy(destination))
    const saved = entry(destination, id, kind)
    subscribers[kind].forEach(notify => notify())
    return saved
  } catch (error) {
    if (folder.exists) folder.delete()
    throw error
  } finally {
    if (converted?.exists) converted.delete()
  }
}

function remoteID(source: RemoteFile, kind: Kind) {
  return 'remote-' + sha256(JSON.stringify([kind, source.uri, Object.entries(source.headers).sort()]))
}

export function saveRemote(source: RemoteFile, kind: Kind, workspace?: WorkspaceFileSource): Promise<LibraryEntry> {
  const id = workspace
    ? 'workspace-' + sha256(JSON.stringify([workspace.account, workspace.path]))
    : remoteID(source, kind)
  const pending = remoteSaves.get(id)
  if (pending) return pending
  const saveFile = async () => {
    const folder = new Directory(root(kind), id)
    if (!workspace) {
      const existing = firstFile(folder)
      if (existing) return entry(existing, id, kind)
    }
    const staging = new Directory(Paths.cache, 'downloads', nonce())
    staging.create({ intermediates: true })
    let keepBackup = false
    try {
      const download = await File.downloadFileAsync(source.uri, new File(staging, safeName(source.name)), { headers: source.headers })
      const existing = firstFile(folder)
      if (!existing) return await save({ ...source, uri: download.uri }, kind, id, true)

      // iOS removes the destination before an overwrite. Retain a copy until
      // the replacement succeeds so a failed move does not discard the file.
      const backup = new File(staging, nonce())
      await existing.copy(backup)
      try {
        await download.move(existing, { overwrite: true })
      } catch (error) {
        if (!existing.exists) {
          keepBackup = true
          await backup.move(existing)
          keepBackup = false
        }
        throw error
      }
      const saved = entry(existing, id, kind)
      subscribers[kind].forEach(notify => notify())
      return saved
    } finally {
      if (!keepBackup && staging.exists) staging.delete()
    }
  }
  const task = saveFile().finally(() => { remoteSaves.delete(id) })
  remoteSaves.set(id, task)
  return task
}

export function shareLibraryItem(item: LibraryEntry) {
  return Sharing.shareAsync(item.uri, { dialogTitle: item.name })
}

export async function shareLibrarySelection(items: LibraryEntry[]) {
  if (!items.length) return
  let urls = items.map(item => item.uri)
  let staging: Directory | undefined
  try {
    if (Platform.OS === 'android') {
      // Android grants receiving apps access through the share provider's cache.
      const cache = new Directory(Paths.cache, 'library-share')
      cache.create({ intermediates: true, idempotent: true })
      const cutoff = Date.now() - 24 * 60 * 60 * 1000
      for (const folder of cache.list()) {
        if (folder instanceof Directory && parseInt(folder.name.split('-')[0], 36) < cutoff) folder.delete()
      }
      staging = new Directory(cache, nonce())
      staging.create()
      const copies = []
      for (const item of items) {
        const folder = new Directory(staging, item.id)
        folder.create()
        const target = new File(folder, item.name)
        await new File(item.uri).copy(target)
        copies.push(target.uri)
      }
      urls = copies
    }
    await Share.open({
      ...(urls.length === 1 ? { url: urls[0] } : { urls }),
      ...(items[0].kind === 'image' ? { type: 'image/*' } : {}),
      failOnCancel: false,
    })
    // The Android recipient may still be reading its cache files after the chooser closes.
  } catch (error) {
    if (staging?.exists) staging.delete()
    throw error
  }
}

export async function saveToDevicePhotos(item: LibraryEntry): Promise<boolean> {
  // Android 11+ can add an app-owned asset without reading the user's library.
  if (Platform.OS === 'ios' || (Platform.OS === 'android' && Number(Platform.Version) < 30)) {
    const permission = await requestPermissionsAsync(true)
    if (!permission.granted) return false
  }
  await Asset.create(item.uri)
  return true
}

export function deleteLibrarySelection(items: LibraryEntry[]) {
  const deleted: string[] = []
  let failed = false
  for (const item of items) {
    const folder = new Directory(root(item.kind), item.id)
    try {
      if (folder.exists) folder.delete()
      deleted.push(item.id)
    } catch {
      failed = true
    }
  }
  for (const kind of new Set(items.map(item => item.kind))) subscribers[kind].forEach(notify => notify())
  return { deleted, failed }
}

export async function pickPhotos() {
  const result = await Photos.launchImageLibraryAsync({ mediaTypes: ['images'], allowsMultipleSelection: true, quality: 1 })
  if (result.canceled) return []
  return Promise.all(result.assets.map(photo => save({ uri: photo.uri, name: photo.fileName || new File(photo.uri).name || 'photo' }, 'image')))
}

export async function pickFiles() {
  const result = await Documents.getDocumentAsync({ multiple: true, copyToCacheDirectory: true })
  if (result.canceled) return []
  return Promise.all(result.assets.map(file =>
    save(file, file.mimeType?.startsWith('image/') || photoExtensions.test(file.name) ? 'image' : 'file'),
  ))
}

export function addAttachments(existing: LibraryEntry[], added: LibraryEntry[]) {
  const all = [...new Map([...existing, ...added].map(file => [file.id, file])).values()]
  const mib = 1024 * 1024
  if (all.some(item => item.size > (item.kind === 'image' ? 50 : 32) * mib)) throw new Error('单张图片最多 50 MB，单个文件最多 32 MB。')
  if (all.filter(item => item.kind === 'image').length > 20 || all.filter(item => item.kind === 'file').length > 8) throw new Error('每条消息至多添加 20 张图片和 8 个文件。')
  if (all.reduce((total, item) => total + item.size, 0) > 200 * mib) throw new Error('附件总大小上限为 200 MB。')
  return all
}

function avatarFolder(account: string) {
  return new Directory(Paths.document, 'avatars', account)
}

export function getAvatar(account: string) {
  const folder = avatarFolder(account)
  if (!folder.exists) return undefined
  return folder.list().filter((item): item is File => item instanceof File).sort((a, b) => b.name.localeCompare(a.name))[0]?.uri
}

export async function pickAvatar(account: string, camera: boolean) {
  if (camera) {
    const permission = await Photos.requestCameraPermissionsAsync()
    if (!permission.granted) throw new Error('请在系统设置中允许 Go Bot 访问相机。')
  }
  const options: Photos.ImagePickerOptions = { mediaTypes: ['images'], allowsEditing: true, aspect: [1, 1], quality: 0.85 }
  const result = camera ? await Photos.launchCameraAsync(options) : await Photos.launchImageLibraryAsync(options)
  if (result.canceled) return undefined
  const source = new File(result.assets[0].uri)
  const folder = avatarFolder(account)
  folder.create({ intermediates: true, idempotent: true })
  const previous = folder.list()
  const extension = source.extension.replace(/[^.a-z0-9]/gi, '') || '.jpg'
  const file = new File(folder, nonce() + extension)
  await source.copy(file)
  previous.forEach(item => { if (item instanceof File) item.delete() })
  return file.uri
}

export async function shareDownload(url: string, headers: Record<string, string>, name: string) {
  const folder = new Directory(Paths.cache, 'downloads', nonce())
  folder.create({ intermediates: true })
  try {
    const download = await File.downloadFileAsync(url, new File(folder, safeName(name)), { headers })
    await Sharing.shareAsync(download.uri, { dialogTitle: name })
  } finally {
    folder.delete()
  }
}

export function formatSize(bytes: number) {
  return bytes >= 1024 * 1024 ? (bytes / 1024 / 1024).toFixed(1) + ' MB'
    : bytes >= 1024 ? Math.ceil(bytes / 1024) + ' KB' : bytes + ' B'
}
