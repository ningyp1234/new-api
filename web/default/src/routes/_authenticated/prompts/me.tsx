// P0 D8 — newapi 主框架内的"我的 Prompt 历史"路由
//
// 设计：React 路由只做一件事 — 渲染一个 iframe 内嵌 /landing/prompts.html
//
// 为什么走 iframe 而不是重新写 React 版本：
//   1. /landing/prompts.html 已经 production-ready（含登录 + DLP 联动 + 详情弹窗 + 复制模板）
//   2. 避免 React + HTML 双实现的维护负担
//   3. iframe 与父 frame 同源 → cookie 自动共享 → 不需要重复 auth
//   4. 把 user_id 通过 URL ?uid=X 传给 iframe，HTML 端会同步写 localStorage 完成登录态
//
// embedded=1 让 HTML 知道自己在 iframe 里，自动隐藏冗余 header
import { createFileRoute } from '@tanstack/react-router'
import { useAuthStore } from '@/stores/auth-store'

function PromptHistoryPage() {
  const user = useAuthStore((s) => s.auth.user)
  const uid = user?.id ?? ''

  // 拼 iframe URL — embedded=1 让 HTML 隐藏自己的 header
  const iframeSrc = `/landing/prompts.html?uid=${uid}&embedded=1`

  return (
    <div className='flex flex-col h-full w-full'>
      <iframe
        src={iframeSrc}
        title='My Prompt History'
        // 撑满父容器的可视区域 — newapi 的内容区高度由 layout 控制
        // calc 让 iframe 占满除顶部 header 外的所有空间
        style={{
          width: '100%',
          height: 'calc(100vh - 56px)',
          border: 'none',
          background: 'transparent',
        }}
        // 允许同源 storage / clipboard / popups
        sandbox='allow-same-origin allow-scripts allow-popups allow-forms allow-modals'
      />
    </div>
  )
}

export const Route = createFileRoute('/_authenticated/prompts/me')({
  component: PromptHistoryPage,
})
