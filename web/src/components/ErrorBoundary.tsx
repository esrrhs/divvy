import { Component, type ErrorInfo, type ReactNode } from 'react'

interface Props {
  children: ReactNode
}

interface State {
  error: Error | null
}

// A render error used to unmount the whole app into a silent white screen:
// no message, no error, nothing. The user's only recovery was a blind reload.
// The boundary keeps the shell alive — show what broke and a way back.
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('render error:', error, info.componentStack)
  }

  render() {
    if (!this.state.error) return this.props.children
    return (
      <div className="flex h-full items-center justify-center bg-ink-900 p-6">
        <div className="max-w-lg space-y-3 rounded border border-line bg-ink-850 p-5">
          <div className="text-[15px] font-semibold text-slate-100">界面渲染出错了</div>
          <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-all text-[12px] leading-relaxed text-rose-300">
            {this.state.error.message || String(this.state.error)}
          </pre>
          <div className="flex gap-2">
            <button
              type="button"
              className="btn px-3 py-1 text-[12px]"
              onClick={() => window.location.reload()}
            >
              重载页面
            </button>
            <button
              type="button"
              className="btn px-3 py-1 text-[12px]"
              onClick={() => this.setState({ error: null })}
            >
              尝试恢复
            </button>
          </div>
        </div>
      </div>
    )
  }
}
