import { useState } from 'react'
import { parseExplanation } from '../lib/runtime-explanations'

interface Props {
  data: any // ApprovalRecord or RuntimeEvent
  compact?: boolean
  className?: string
  defaultExpanded?: boolean
}

export function RuntimeExplanationPanel({ data, compact = false, className, defaultExpanded = false }: Props) {
  const [expanded, setExpanded] = useState(defaultExpanded)
  
  if (!data) return null
  
  const explanation = parseExplanation(data)

  const cx = (...classes: Array<string | false | null | undefined>) => classes.filter(Boolean).join(' ')

  const ChevronIcon = ({ className }: { className?: string }) => (
    <svg 
      xmlns="http://www.w3.org/2000/svg" 
      viewBox="0 0 20 20" 
      fill="currentColor" 
      className={cx("w-5 h-5 transition-transform duration-200", className)}
    >
      <path fillRule="evenodd" d="M5.23 7.21a.75.75 0 011.06.02L10 11.168l3.71-3.938a.75.75 0 111.08 1.04l-4.25 4.5a.75.75 0 01-1.08 0l-4.25-4.5a.75.75 0 01.02-1.06z" clipRule="evenodd" />
    </svg>
  )

  if (compact) {
    return (
      <div className={cx("bg-gray-800/50 rounded-md border border-gray-700/50 overflow-hidden text-sm", className)}>
        <button 
          onClick={() => setExpanded(!expanded)}
          className="w-full flex items-center justify-between p-3 text-left hover:bg-gray-700/30 transition-colors"
        >
          <div className="flex items-center gap-2">
            <span className="text-gray-400">Why was this blocked?</span>
            <span className="text-gray-300 truncate max-w-xs md:max-w-md">{explanation.summary}</span>
          </div>
          <ChevronIcon className={expanded ? "rotate-180" : ""} />
        </button>
        
        {expanded && (
          <div className="p-4 border-t border-gray-700/50 space-y-3 bg-gray-800/80">
            {explanation.nextStep && (
              <div>
                <span className="text-gray-400 block text-xs uppercase tracking-wider mb-1">What you can do</span>
                <span className="text-gray-200">{explanation.nextStep}</span>
              </div>
            )}
            {explanation.identifiers.length > 0 && (
              <div className="pt-2">
                 <span className="text-gray-500 block text-xs uppercase tracking-wider mb-1">Details</span>
                 <div className="flex flex-wrap gap-3">
                   {explanation.identifiers.map((id, i) => (
                     <div key={i} className="text-xs">
                       <span className="text-gray-500 mr-1">{id.label}:</span>
                       <span className="text-gray-400 font-mono">{id.value}</span>
                     </div>
                   ))}
                 </div>
              </div>
            )}
          </div>
        )}
      </div>
    )
  }

  return (
    <div className={cx("bg-gray-800 rounded-lg border border-gray-700 overflow-hidden shadow-sm mt-4", className)}>
      <button 
        onClick={() => setExpanded(!expanded)}
        className="w-full flex items-center justify-between p-4 text-left hover:bg-gray-700/50 transition-colors group"
      >
        <div className="flex items-center gap-3">
          <div className="bg-amber-500/10 text-amber-500 p-1.5 rounded-md group-hover:bg-amber-500/20 transition-colors">
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
              <path fillRule="evenodd" d="M18 10a8 8 0 11-16 0 8 8 0 0116 0zm-8-5a.75.75 0 01.75.75v4.5a.75.75 0 01-1.5 0v-4.5A.75.75 0 0110 5zm0 10a1 1 0 100-2 1 1 0 000 2z" clipRule="evenodd" />
            </svg>
          </div>
          <div>
            <h4 className="text-sm font-medium text-gray-200">Why was this blocked?</h4>
            {!expanded && <p className="text-xs text-gray-400 mt-0.5 truncate max-w-[200px] sm:max-w-md">{explanation.summary}</p>}
          </div>
        </div>
        <ChevronIcon className={expanded ? "rotate-180 text-gray-400" : "text-gray-500 group-hover:text-gray-400"} />
      </button>

      {expanded && (
        <div className="px-4 pb-4 animate-in slide-in-from-top-2 fade-in duration-200">
          <div className="border-t border-gray-700 pt-4 space-y-4">
            
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              <div>
                <span className="text-gray-400 block text-xs uppercase tracking-wider mb-1">Why</span>
                <p className="text-gray-200 text-sm">{explanation.summary}</p>
              </div>
              
              {explanation.nextStep && (
                <div>
                  <span className="text-gray-400 block text-xs uppercase tracking-wider mb-1">What you can do</span>
                  <p className="text-gray-200 text-sm">{explanation.nextStep}</p>
                </div>
              )}
            </div>

            <div className="grid grid-cols-2 sm:grid-cols-4 gap-4 bg-gray-900/50 p-3 rounded-md border border-gray-800">
               {explanation.toolName && (
                  <div>
                    <span className="text-gray-500 block text-xs uppercase tracking-wider mb-1">Tool</span>
                    <span className="text-gray-300 text-sm font-mono truncate block" title={explanation.toolName}>{explanation.toolName}</span>
                  </div>
               )}
               {explanation.target && (
                  <div className="col-span-2 sm:col-span-1">
                    <span className="text-gray-500 block text-xs uppercase tracking-wider mb-1">Target</span>
                    <span className="text-gray-300 text-sm font-mono truncate block" title={explanation.target}>{explanation.target}</span>
                  </div>
               )}
               {explanation.risk && (
                  <div>
                    <span className="text-gray-500 block text-xs uppercase tracking-wider mb-1">Risk</span>
                    <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-gray-800 text-gray-300 border border-gray-700">
                      {explanation.risk}
                    </span>
                  </div>
               )}
            </div>
            
            {explanation.identifiers.length > 0 && (
              <div className="pt-2 border-t border-gray-800/50">
                 <div className="flex flex-wrap gap-x-6 gap-y-2">
                   {explanation.identifiers.map((id, i) => (
                     <div key={i} className="text-xs flex flex-col">
                       <span className="text-gray-500 uppercase tracking-wider" style={{fontSize: '0.65rem'}}>{id.label}</span>
                       <span className="text-gray-400 font-mono mt-0.5">{id.value}</span>
                     </div>
                   ))}
                 </div>
              </div>
            )}
            
          </div>
        </div>
      )}
    </div>
  )
}
