import { useState, type FormEvent } from 'react'
import { LoaderCircle } from 'lucide-react'
import { APIError, getModelSettings, registerAccount } from './api/controller'
import { HarnessAvatar } from './HarnessAvatar'

export function AccountEntry({ onOpen }: { onOpen: (userId: string, created: boolean) => void }) {
  const [create, setCreate] = useState(false)
  const [name, setName] = useState('')
  const [phase, setPhase] = useState<'creating' | 'starting'>()
  const [error, setError] = useState('')

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError('')
    try {
      if (create) {
        setPhase('creating')
        await registerAccount(name)
        onOpen(name, true)
      } else {
        setPhase('starting')
        await getModelSettings(name)
        onOpen(name, false)
      }
    } catch (error) {
      if (error instanceof APIError && error.status === 404) {
        setError('Account not found. Create it or check the name.')
      } else if (error instanceof APIError && error.message) {
        setError(error.message)
      } else {
        setError('Could not open the account. Try again.')
      }
    } finally {
      setPhase(undefined)
    }
  }

  return (
    <main className="account-page">
      <form className="account-card" onSubmit={submit}>
        <div className="brand"><HarnessAvatar className="brand-mark" />Go Harness</div>
        <h1>{create ? 'Create an account' : 'Open your account'}</h1>
        <fieldset disabled={Boolean(phase)}>
          <label>Account name
            <input autoComplete="username" autoCapitalize="none" spellCheck={false}
              required pattern="[a-z0-9][a-z0-9_\x2d]{0,63}" value={name} onChange={event => setName(event.target.value)}
              title="Use 1–64 lowercase letters, digits, underscores or hyphens. Start with a letter or digit." />
          </label>
          <button className="account-submit" type="submit">{create ? 'Create account' : 'Open account'}</button>
        </fieldset>
        {phase && <p className="account-status" role="status"><LoaderCircle size={16} />{phase === 'creating' ? 'Creating account…' : 'Opening your account…'}</p>}
        {error && <p className="account-error" role="alert">{error}</p>}
        <button className="account-switch" type="button" disabled={Boolean(phase)} onClick={() => { setCreate(!create); setError('') }}>
          {create ? 'Open an existing account' : 'Create an account'}
        </button>
      </form>
    </main>
  )
}
