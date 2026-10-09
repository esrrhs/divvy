import { useEffect } from 'react'
import { CenterMain } from './components/CenterMain'
import { RightPanel } from './components/RightPanel'
import { Sidebar } from './components/Sidebar'
import { TopBar } from './components/TopBar'
import { AskDialog } from './components/AskDialog'
import { ActionToast } from './components/ActionToast'
import { getToken } from './api/client'
import { useStore } from './store'

export default function App() {
  const currentId = useStore((s) => s.currentId)
  const live = useStore((s) => s.live)
  const sseStatus = useStore((s) => s.sseStatus)
  const connect = useStore((s) => s.connect)
  const disconnect = useStore((s) => s.disconnect)
  const refreshSessions = useStore((s) => s.refreshSessions)
  const token = getToken()

  // Subscribe to a session's event stream whenever it is opened. The fetch
  // (openSession) happens on click/create; here we only manage the SSE.
  useEffect(() => {
    if (!currentId || !token) {
      disconnect()
      return
    }
    connect(currentId, token)
    return () => disconnect()
    // connect/disconnect are stable zustand actions.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentId, token])

  // Keep the session directory fresh while runs progress.
  useEffect(() => {
    const t = setInterval(() => void refreshSessions(), 5000)
    return () => clearInterval(t)
  }, [refreshSessions])

  return (
    <div className="flex h-full flex-col">
      <TopBar live={live} sseStatus={sseStatus} />
      <div className="flex min-h-0 flex-1">
        <Sidebar
          onNewSession={() =>
            useStore.setState({ currentId: null, live: null, tree: null, selectedNodeId: null, fileView: null, events: [] })
          }
        />
        <main className="flex min-w-0 flex-1 flex-col bg-ink-900">
          <CenterMain />
        </main>
        <RightPanel />
      </div>
      <AskDialog />
      <ActionToast />
    </div>
  )
}
