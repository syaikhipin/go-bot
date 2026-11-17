import { createRoot } from 'react-dom/client'
import { App } from './App'
import { LocaleProvider } from './locale'
import './styles/app.css'

createRoot(document.getElementById('root')!).render(<LocaleProvider><App /></LocaleProvider>)
