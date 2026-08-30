import { render } from 'preact'
import { App } from './app'
import './styles/tokens.css'
import './styles/base.css'
import './styles/components.css'

render(<App />, document.getElementById('app') as HTMLElement)
