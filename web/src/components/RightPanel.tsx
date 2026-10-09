import { useState } from 'react'
import { BranchIcon, DocIcon, TerminalIcon } from './icons'
import { NodeDetail } from './NodeDetail'
import { LogView } from './LogView'
import { useStore } from '../store'

type Tab = 'node' | 'diff' | 'log'

const tabs = [
  ['node', BranchIcon, '节点详情'],
  ['diff', DocIcon, 'Diff 审批'],
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
        <Placeholder icon={<DocIcon width={18} height={18} />} text="叶子进入人工审批时，这里展示文件清单与 unified diff。" />
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
