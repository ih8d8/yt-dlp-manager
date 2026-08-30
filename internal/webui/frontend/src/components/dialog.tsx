import { Component } from 'preact'

interface DialogProps {
  title: string
  onClose: () => void
  children: preact.ComponentChildren
}
/**
 * Modal dialog with focus trap, Escape-to-close, and focus restoration.
 * The top overlay always closes on Escape (spec §14).
 */
export class Dialog extends Component<DialogProps> {
  box: HTMLDivElement | null = null
  previouslyFocused: HTMLElement | null = null

  componentDidMount(): void {
    this.previouslyFocused = document.activeElement as HTMLElement | null
    // Prefer an explicitly marked autofocus target (e.g. the Add dialog's
    // URL field); otherwise focus the first focusable element.
    const initial =
      this.box?.querySelector<HTMLElement>('[autofocus]') ??
      this.box?.querySelectorAll<HTMLElement>(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
      )[0]
    initial?.focus()
  }

  componentWillUnmount(): void {
    // Restoring synchronously here lands on <body>: at this point the dialog's
    // own subtree is being torn down and the browser has not settled focus, so
    // the focus() call is undone moments later. Defer past the teardown and
    // re-check the node is still connected before using it.
    const target = this.previouslyFocused
    if (!target) return
    requestAnimationFrame(() => {
      if (target.isConnected) target.focus()
    })
  }

  onKey = (e: KeyboardEvent): void => {
    if (e.key === 'Escape') {
      e.stopPropagation()
      this.props.onClose()
      return
    }
    if (e.key === 'Tab' && this.box) {
      const items = Array.from(
        this.box.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        )
      ).filter((el) => !el.hasAttribute('disabled'))
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
  }

  render({ title, children, onClose }: DialogProps) {
    return (
      <div class="overlay" onKeyDown={this.onKey}>
        <div
          class="dialog"
          role="dialog"
          aria-modal="true"
          aria-label={title}
          ref={(el) => {
            this.box = el as HTMLDivElement | null
          }}
        >
          <button class="dialog-close" aria-label="Close" onClick={onClose}>
            ✕
          </button>
          <h2>{title}</h2>
          {children}
        </div>
      </div>
    )
  }
}
