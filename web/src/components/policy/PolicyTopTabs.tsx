export type PolicyTopTab = 'tools' | 'accounts'

export default function PolicyTopTabs({
  activeTab,
  onTabChange,
  toolCount,
  accountCount,
}: {
  activeTab: PolicyTopTab
  onTabChange: (tab: PolicyTopTab) => void
  toolCount: number
  accountCount: number
}) {
  const tabs: Array<{ id: PolicyTopTab; label: string; count: number }> = [
    { id: 'tools', label: 'Tool Controls', count: toolCount },
    { id: 'accounts', label: 'Account Controls', count: accountCount },
  ]

  return (
    <div className="policy-tabs-track" role="tablist" aria-label="Policy sections">
      {tabs.map(tab => {
        const active = activeTab === tab.id
        return (
          <button
            key={tab.id}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onTabChange(tab.id)}
            className={`policy-tab ${active ? 'policy-tab--active' : 'policy-tab--inactive'}`}
          >
            {tab.label}
            <span className="policy-tab-count">{tab.count}</span>
          </button>
        )
      })}
    </div>
  )
}
