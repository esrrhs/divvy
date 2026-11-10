import { useState } from 'react'
import { BranchIcon, DocIcon, TerminalIcon } from './icons'
import { NodeDetail } from './NodeDetail'
import { LogView } from './LogView'
import { FileDiffList } from './FileDiffView'
import { useStore } from '../store'

type Tab = 'node' | 'diff' | 'log'

const tabs = [
  ['node', BranchIcon, '节点详情'],
  ['diff', DocIcon, '本轮差异'],
  ['log', TerminalIcon, '日志'],
] as const

export function RightPanel() {
  const [tab, setTab] = useState<Tab>('node')
  const tree = useStore((s) => s.tree)
  const selectedNodeId = useStore((s) => s.selectedNodeId)
  const currentId = useStore((s) => s.currentId)
  const node = tree && selectedNodeId ? tree.nodes[selectedNodeId] : undefined

  return (
    <aside className="flex h-full w-[340px] shrink-0 flex-col border-l border-line bg-ink-850">
      <div className="flex border-b border-line">
        {tabs.map(([key, Icon, label]) => (
          <button
            key={key}
            onClick={() => setTab(key)}
            className={`flex flex-1 items-center justify-center gap-1.5 py-2.5 text-[12px] transition ${
              tab === key
                ? 'border-b-2 border-brand-500 text-slate-200'
                : 'text-slate-500 hover:text-slate-400'
            }`}
          >
            <Icon width={13} height={13} />
            {label}
          </button>
        ))}
      </div>

      {tab === 'node' ? (
        node ? (
          <NodeDetail node={node} />
        ) : (
          <Placeholder
            icon={<BranchIcon width={18} height={18} />}
            text="选择任务树中的节点，查看契约、DoD、错误历史与 token 用量。"
          />
        )
      ) : tab === 'diff' ? (
        <TurnDiffPanel />
      ) : currentId ? (
        <LogView sessionId={currentId} />
      ) : (
        <Placeholder icon={<TerminalIcon width={18} height={18} />} text="打开会话后可分页查看运行日志。" />
      )}
    </aside>
  )
}

function Placeholder({ icon, text }: { icon: React.ReactNode; text: string }) {
  return (
    <div className="flex flex-1 items-center justify-center px-8 text-center">
      <div>
        <div className="mx-auto mb-3 flex h-10 w-10 items-center justify-center rounded-full border border-line bg-ink-800 text-slate-600">
          {icon}
        </div>
        <p className="text-[12.5px] text-slate-500">{text}</p>
      </div>
    </div>
  )
}

function TurnDiffPanel() {
  const turnChanges = useStore((s) => s.turnChanges)
  const conversation = useStore((s) => s.conversation)
  const openFile = useStore((s) => s.openFile)
  const loadTurnChanges = useStore((s) => s.loadTurnChanges)
  const applyTurnHunks = useStore((s) => s.applyTurnHunks)
  const actionError = useStore((s) => s.actionError)
  const changes = turnChanges?.changes ?? []

  if (!conversation) {
    return (
      <Placeholder
        icon={<DocIcon width={18} height={18} />}
        text="打开对话后，这里按文件列出最近一轮的改动。叶子人工审批仍在中间区域。"
      />
    )
  }
  if (changes.length === 0) {
    return (
      <div className="flex flex-1 flex-col overflow-auto px-3 py-3">
        <p className="text-[12.5px] text-slate-500">这一轮还没有可审阅的文件改动。执行结束后可在这里按文件查看差异。</p>
        <button
          type="button"
          className="btn mt-3 self-start px-2 py-1 text-[11.5px]"
          onClick={() => void loadTurnChanges()}
        >
          重新加载
        </button>
      </div>
    )
  }
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-auto px-3 py-3">
      <p className="mb-2 text-[11.5px] text-slate-500">
        本轮改动 {changes.length} 个文件。点文件名可在中间查看器里对照差异。最新一轮结束后可在这里按块接受或拒绝并写回工作区；叶子审批仍在计划通过后的中间卡片里，两边不会互相绕过。
      </p>
      {actionError && <p className="mb-2 text-[12px] text-rose-300">{actionError}</p>}
      <ul className="mb-3 space-y-1">
        {changes.map((c) => (
          <li key={c.path}>
            <button
              type="button"
              className="w-full truncate rounded px-1 py-0.5 text-left font-mono text-[11.5px] text-slate-300 hover:bg-ink-800"
              onClick={() => openFile(c.path, turnChanges?.sessionId, { preferDiff: true })}
            >
              <span className="text-slate-500">{c.status}</span> {c.path}
            </button>
          </li>
        ))}
      </ul>
      <FileDiffList
        changes={changes}
        editable={!!turnChanges?.editable}
        reason={turnChanges?.reason}
        mode={turnChanges?.editable ? 'apply' : 'readonly'}
        onApply={(payload) => applyTurnHunks(payload)}
      />
    </div>
  )
}
