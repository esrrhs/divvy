import { useState } from 'react'
import { useStore } from '../store'
import { PauseIcon, PlusIcon, SpinnerIcon, XIcon } from './icons'

export function RunControls() {
  const phase = useStore((s) => s.live?.phase)
  const pause = useStore((s) => s.pause)
  const abort = useStore((s) => s.abort)
  const addInstruction = useStore((s) => s.addInstruction)
  const redoNode = useStore((s) => s.redoNode)
  const selectedNode = useStore((s) =>
    s.selectedNodeId ? s.tree?.nodes[s.selectedNodeId] : undefined,
  )

  const [addOpen, setAddOpen] = useState(false)
  const [instruction, setInstruction] = useState('')
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)

  const running = phase === 'running' || phase === 'ask'
  const live = phase !== undefined && phase !== 'done' && phase !== 'failed' && phase !== 'paused'

  async function act(fn: () => Promise<boolean>, okMsg?: string): Promise<boolean> {
    setBusy(true)
    setNotice(null)
    const ok = await fn()
    setBusy(false)
    if (ok && okMsg) setNotice(okMsg)
    return ok
  }

  async function submitAdd() {
    if (!instruction.trim()) return
    const ok = await act(() => addInstruction(instruction))
    if (ok) {
      setInstruction('')
      setAddOpen(false)
    }
  }

  return (
    <div className="shrink-0 border-b border-line bg-ink-850/80 px-5 py-2">
      <div className="flex items-center gap-2">
        <button
          className="btn-ghost py-1 text-[12px]"
          disabled={busy || !running}
          title="保存任务树并停止，可随时续跑"
          onClick={() => void act(pause)}
        >
          {busy ? <SpinnerIcon width={12} height={12} /> : <PauseIcon width={12} height={12} />}
          暂停
        </button>
        <button
          className="btn-ghost py-1 text-[12px]"
          disabled={busy || !live}
          title="放弃当前会话"
          onClick={() => {
            if (confirm('确定中止当前会话？计划/进度会保留在磁盘但不再执行。')) void act(abort)
          }}
        >
          <XIcon width={12} height={12} />
          中止
        </button>
        <button
          className="btn-ghost py-1 text-[12px]"
          disabled={busy || phase !== 'running'}
          title="把新需求作为叶子加入任务树"
          onClick={() => setAddOpen((v) => !v)}
        >
          <PlusIcon width={12} height={12} />
          加叶子
        </button>
        <button
          className="btn-ghost py-1 text-[12px]"
          disabled={busy || phase !== 'running' || !selectedNode}
          title={`重做选中节点${selectedNode ? ` (${selectedNode.id})` : ''}`}
          onClick={() => {
            if (selectedNode) void act(() => redoNode(selectedNode.id), `已请求重做 ${selectedNode.id}`)
          }}>
          重做节点{selectedNode ? ` ${selectedNode.id}` : ''}
        </button>

        {notice && <span className="ml-2 text-[11.5px] text-emerald-400/90">{notice}</span>}
        <span className="ml-auto text-[11px] text-slate-600">
          {!running && live ? '当前阶段不能执行运行控制（审批/问答中除外）' : ''}
        </span>
      </div>

      {addOpen && (
        <div className="mt-2 flex items-start gap-2">
          <input
            className="input flex-1 py-1.5 text-[12.5px]"
            placeholder="新叶子的指令，例如：再加一个 /metrics 端点"
            value={instruction}
            onChange={(e) => setInstruction(e.target.value)}
            autoFocus
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submitAdd()
              if (e.key === 'Escape') setAddOpen(false)
            }}
          />
          <button className="btn-primary px-3 py-1.5 text-[12px]" disabled={busy || !instruction.trim()} onClick={() => void submitAdd()}>
            加入
          </button>
          <button className="btn-ghost px-2 py-1.5 text-[12px]" onClick={() => setAddOpen(false)}>
            取消
          </button>
        </div>
      )}
    </div>
  )
}
