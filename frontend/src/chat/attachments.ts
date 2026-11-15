const imageTypes = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/gif'])
const maxBytes = 200 * 1024 ** 2

export function isMessageImage(file: File) {
  return imageTypes.has(file.type)
}

export function addMessageFiles(current: File[], incoming: File[]) {
  const files = [...current]
  const errors = new Set<string>()
  let images = current.filter(isMessageImage).length
  let total = current.reduce((sum, file) => sum + file.size, 0)
  for (const file of incoming) {
    const image = isMessageImage(file)
    const limit = image ? 50 : 32
    if (image && !file.size) errors.add(`${file.name} is empty.`)
    else if (new TextEncoder().encode(file.name).length > 255) errors.add('An attachment name exceeds 255 bytes.')
    else if (file.size > limit * 1024 ** 2) errors.add(`${file.name} exceeds ${limit} MiB.`)
    else if (image ? images >= 20 : files.length - images >= 8) errors.add('A message can attach up to 20 images and 8 other files.')
    else if (total + file.size > maxBytes) errors.add('Attachments cannot exceed 200 MiB per message.')
    else {
      files.push(file)
      total += file.size
      if (image) images++
    }
  }
  return { files, error: [...errors].join(' ') }
}

export function messageRequest(content: string, files: File[]): RequestInit {
  if (!files.length) {
    return { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ content }) }
  }
  const body = new FormData()
  body.append('content', content)
  for (const file of files) body.append(isMessageImage(file) ? 'images' : 'files', file)
  return { method: 'POST', body }
}
